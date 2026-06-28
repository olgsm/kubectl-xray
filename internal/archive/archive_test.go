package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarOf builds an in-memory tar archive from the given entries.
func tarOf(t *testing.T, entries []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := range entries {
		h := entries[i]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(bodies[h.Name]))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("WriteHeader %q: %v", h.Name, err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(bodies[h.Name])); err != nil {
				t.Fatalf("Write %q: %v", h.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// gzipOf wraps b in a single gzip member.
func gzipOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func sampleArchive(t *testing.T) []byte {
	bodies := map[string]string{
		"./mypod.jstack": "Full thread dump\n",
		"./mypod.hprof":  "JAVA PROFILE 1.0.2\x00\x01binary",
		"nested/x.txt":   "nested body",    // must flatten to x.txt
		"../escape.txt":  "traversal body", // must be confined to dest as escape.txt
	}
	entries := []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "./mypod.jstack", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "./mypod.hprof", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "nested/x.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
	}
	return tarOf(t, entries, bodies)
}

func TestExtractTar(t *testing.T) {
	dest := t.TempDir()

	files, err := extractTar(bytes.NewReader(sampleArchive(t)), dest, nil)
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	// The directory entry is skipped; 4 regular files written.
	if len(files) != 4 {
		t.Fatalf("want 4 files written, got %d: %v", len(files), files)
	}

	// Contents preserved (incl. binary), names flattened to base.
	for base, want := range map[string]string{
		"mypod.jstack": "Full thread dump\n",
		"mypod.hprof":  "JAVA PROFILE 1.0.2\x00\x01binary",
		"x.txt":        "nested body",
		"escape.txt":   "traversal body",
	} {
		got, rerr := os.ReadFile(filepath.Join(dest, base))
		if rerr != nil {
			t.Errorf("reading %s: %v", base, rerr)
			continue
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", base, got, want)
		}
	}

	// Traversal guard: nothing escaped the dest directory.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); err == nil {
		t.Error("traversal: escape.txt was written outside dest")
	}
}

func TestExtractTarBadArchive(t *testing.T) {
	// >512 bytes of non-tar data forces a header-parse error (not a clean EOF).
	garbage := strings.Repeat("x", 600)
	files, err := extractTar(strings.NewReader(garbage), t.TempDir(), nil)
	if err == nil {
		t.Fatalf("want error on invalid archive, got files=%v", files)
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "out.txt")
	if err := writeFile(path, nil, func(w io.Writer) error {
		_, err := w.Write([]byte("hello"))
		return err
	}); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back = %q, %v; want %q", got, err, "hello")
	}

	// Bundle holds secrets: the file must be 0o600, not the umask default.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %v, want 0600", perm)
	}

	// A fill error must leave no file (and no leftover temp) at the path.
	bad := filepath.Join(dir, "bad.txt")
	if err := writeFile(bad, nil, func(io.Writer) error { return fs.ErrInvalid }); err == nil {
		t.Error("want error from failing fill")
	}
	if _, err := os.Stat(bad); err == nil {
		t.Error("partial file left behind on fill error")
	}
}

func TestWriteAndExtract(t *testing.T) {
	gz := gzipOf(t, sampleArchive(t))

	// Write counts regular-file entries and lands the bytes on disk.
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	n, err := Write(bytes.NewReader(gz), out, 0)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 4 {
		t.Errorf("Write count = %d, want 4", n)
	}

	// Extract unpacks the same stream into a directory.
	dest := t.TempDir()
	files, err := Extract(bytes.NewReader(gz), dest, 0)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(files) != 4 {
		t.Errorf("Extract files = %d, want 4", len(files))
	}
}

func TestMaxSize(t *testing.T) {
	gz := gzipOf(t, sampleArchive(t))

	// A 1-byte budget can't fit the compressed bundle.
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if _, err := Write(bytes.NewReader(gz), out, 1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Write err = %v, want ErrTooLarge", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("over-budget bundle left on disk")
	}

	// Extract uncompressed bodies total ~58 bytes; a 10-byte budget overflows.
	if _, err := Extract(bytes.NewReader(gz), t.TempDir(), 10); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Extract err = %v, want ErrTooLarge", err)
	}
}
