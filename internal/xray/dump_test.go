package xray

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildJVMDumpScript(t *testing.T) {
	const name = "mypod"
	tests := []struct {
		label                   string
		thread, histogram, heap bool
		wantContains            []string
		wantAbsent              []string
	}{
		{
			label: "all steps", thread: true, histogram: true, heap: true,
			wantContains: []string{"jstack 1", "GC.class_histogram", "jmap -dump:live", "mypod.jstack", "mypod.hprof", "/proc/1/root/tmp/mypod.hprof"},
		},
		{
			label: "thread only", thread: true,
			wantContains: []string{"jstack 1", "mypod.jstack"},
			wantAbsent:   []string{"GC.class_histogram", "jmap"},
		},
		{
			label: "histogram only", histogram: true,
			wantContains: []string{"GC.class_histogram"},
			wantAbsent:   []string{"jstack", "jmap"},
		},
		{
			label: "heap only", heap: true,
			wantContains: []string{"jmap -dump:live"},
			wantAbsent:   []string{"jstack", "GC.class_histogram"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := buildJVMDumpScript(tt.thread, tt.histogram, tt.heap, name)
			if !strings.HasPrefix(got, `W="$(mktemp -d)"; `) {
				t.Errorf("script must set up a work dir first; got %q", got)
			}
			if !strings.HasSuffix(got, `tar czf - -C "$W" .`) {
				t.Errorf("script must end by gzip-tarring the work dir to stdout; got %q", got)
			}
			for _, s := range tt.wantContains {
				if !strings.Contains(got, s) {
					t.Errorf("want %q in script; got %q", s, got)
				}
			}
			for _, s := range tt.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("did not want %q in script; got %q", s, got)
				}
			}
		})
	}
}

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

func TestExtractTar(t *testing.T) {
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
	dest := t.TempDir()

	files, err := extractTar(bytes.NewReader(tarOf(t, entries, bodies)), dest)
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	// The directory entry is skipped; 4 regular files written.
	if len(files) != 4 {
		t.Fatalf("want 4 files written, got %d: %v", len(files), files)
	}

	// Contents preserved (incl. binary), names flattened to base.
	for base, want := range map[string]string{
		"mypod.jstack": bodies["./mypod.jstack"],
		"mypod.hprof":  bodies["./mypod.hprof"],
		"x.txt":        bodies["nested/x.txt"],
		"escape.txt":   bodies["../escape.txt"],
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
	files, err := extractTar(strings.NewReader(garbage), t.TempDir())
	if err == nil {
		t.Fatalf("want error on invalid archive, got files=%v", files)
	}
}

func TestWriteFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "out.txt")
	if err := writeFile(path, strings.NewReader("hello")); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back = %q, %v; want %q", got, err, "hello")
	}

	// Creating under a non-existent directory must fail (no silent success).
	if err := writeFile(filepath.Join(dir, "missing", "out.txt"), strings.NewReader("x")); err == nil {
		t.Error("want error writing into a non-existent directory")
	}
}
