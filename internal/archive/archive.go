package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Write streams the gzipped tar from r into a file at path and returns the
// number of regular-file entries it contains (0 means the dump produced
// nothing). It tees the raw bytes to disk while counting entries in a single pass.
func Write(r io.Reader, path string) (n int, err error) {
	err = writeFile(path, func(w io.Writer) error {
		n, err = countTarEntries(io.TeeReader(r, w))
		return err
	})
	return n, err
}

// Extract gunzips one member from r and unpacks its files into dest, returning
// the paths written. The trailing drain reads to the member end so the gzip CRC
// is validated, turning a truncated stream into an error instead of silent partial output.
func Extract(r io.Reader, dest string) ([]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()

	gz.Multistream(false)
	files, err := extractTar(gz, dest)
	if err != nil {
		return files, err
	}
	_, err = io.Copy(io.Discard, gz)
	return files, err
}

// countTarEntries reads one gzipped-tar member and counts its regular-file
// entries. Multistream(false) makes it stop at the member end (not block
// waiting for stream EOF); the trailing drain consumes the gzip footer so the
// CRC is validated and (via a TeeReader) the whole member lands on disk.
func countTarEntries(r io.Reader) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	defer func() { _ = gz.Close() }()

	gz.Multistream(false)
	tr := tar.NewReader(gz)
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, err
		}
		if hdr.Typeflag == tar.TypeReg {
			n++
		}
	}
	_, err = io.Copy(io.Discard, gz) // read to the member end → validate footer
	return n, err
}

// extractTar writes each regular file from the tar stream into dest (flattened),
// returning the paths written.
func extractTar(r io.Reader, dest string) ([]string, error) {
	tr := tar.NewReader(r)
	var written []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return written, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		out := filepath.Join(dest, filepath.Base(hdr.Name))
		if err := writeFile(out, func(w io.Writer) error {
			_, cerr := io.Copy(w, tr)
			return cerr
		}); err != nil {
			return written, err
		}
		written = append(written, out)
	}
	return written, nil
}

// writeFile durably writes content produced by fill to path. Output may hold
// sensitive data, so it is written 0o600 to a sibling temp file, fsync'd, then
// atomically renamed into place — no partial or world-readable file ever
// appears at the real path.
func writeFile(path string, fill func(w io.Writer) error) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	if err = os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err = fill(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
