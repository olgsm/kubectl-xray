package archive

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ErrTooLarge is returned when a write would exceed the byte budget passed to
// Write or Extract.
var ErrTooLarge = errors.New("archive: output exceeds max size")

// Write streams the gzipped tar from r into a file at path and returns the
// number of regular-file entries it contains (0 means the dump produced
// nothing). It tees the raw bytes to disk while counting entries in a single
// pass. maxSize caps the bytes written to disk (0 = unlimited); past it the
// write fails with ErrTooLarge and no file is left at path.
func Write(r io.Reader, path string, maxSize int64) (n int, err error) {
	err = writeFile(path, newLimiter(maxSize), func(w io.Writer) error {
		n, err = countTarEntries(io.TeeReader(r, w))
		return err
	})
	return n, err
}

// Extract gunzips one member from r and unpacks its files into dest, returning
// the paths written. The trailing drain reads to the member end so the gzip CRC
// is validated, turning a truncated stream into an error instead of silent
// partial output. maxSize caps the total bytes written across all files
// (0 = unlimited); past it extraction stops with ErrTooLarge.
func Extract(r io.Reader, dest string, maxSize int64) ([]string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()

	gz.Multistream(false)
	files, err := extractTar(gz, dest, newLimiter(maxSize))
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
// returning the paths written. lim bounds the total bytes written across files.
func extractTar(r io.Reader, dest string, lim *limiter) ([]string, error) {
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
		if err := writeFile(out, lim, func(w io.Writer) error {
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
// appears at the real path. lim bounds the bytes fill may write (nil =
// unlimited); on overflow no file is left at path.
func writeFile(path string, lim *limiter, fill func(w io.Writer) error) (err error) {
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
	if err = fill(lim.writer(f)); err != nil {
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

// limiter tracks a shared byte budget across one Write/Extract operation.
type limiter struct{ remaining int64 }

// newLimiter returns a limiter for max bytes, or nil (unlimited) when max <= 0.
func newLimiter(max int64) *limiter {
	if max <= 0 {
		return nil
	}
	return &limiter{remaining: max}
}

// writer wraps w so writes draw down the budget, failing with ErrTooLarge once
// exhausted. A nil limiter passes w through unchanged.
func (l *limiter) writer(w io.Writer) io.Writer {
	if l == nil {
		return w
	}
	return &capWriter{w: w, l: l}
}

type capWriter struct {
	w io.Writer
	l *limiter
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.l.remaining {
		return 0, ErrTooLarge
	}
	c.l.remaining -= int64(len(p))
	return c.w.Write(p)
}
