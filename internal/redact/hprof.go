package redact

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// HPROF copies a binary heap dump from r to w with the contents of every
// primitive array zeroed: String, byte[] and char[] payloads, BigInteger key
// material, and so on. Lengths are kept, so the object graph, histogram and
// retained sizes analyze the same, and the output is exactly as long as the
// input. Anything it can't parse is an error rather than passed through.
func HPROF(r io.Reader, w io.Writer) error {
	h := &hprof{r: bufio.NewReaderSize(r, 1<<16), w: bufio.NewWriterSize(w, 1<<16)}
	if err := h.run(); err != nil {
		return err
	}
	return h.w.Flush()
}

type hprof struct {
	r    *bufio.Reader
	w    *bufio.Writer
	id   int   // identifier size
	left int64 // bytes left in the current heap dump record
	buf  [16]byte
}

const (
	tagHeapDump        = 0x0C
	tagHeapDumpSegment = 0x1C
)

func (h *hprof) run() error {
	magic, err := h.r.ReadBytes(0)
	if err != nil || !bytes.HasPrefix(magic, []byte("JAVA PROFILE 1.0.")) {
		return errors.New("hprof: not a heap dump")
	}
	if _, err := h.w.Write(magic); err != nil {
		return err
	}
	b, err := h.header(12) // u4 identifier size, u8 timestamp
	if err != nil {
		return unexpected(err)
	}
	h.id = int(binary.BigEndian.Uint32(b))
	if h.id != 4 && h.id != 8 {
		return fmt.Errorf("hprof: unsupported identifier size %d", h.id)
	}
	for {
		b, err := h.header(9) // u1 tag, u4 time, u4 length
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return unexpected(err)
		}
		tag, n := b[0], int64(binary.BigEndian.Uint32(b[5:9]))
		h.left = n
		if tag == tagHeapDump || tag == tagHeapDumpSegment {
			err = h.heapDump()
		} else {
			err = h.copy(n)
		}
		if err != nil {
			return err
		}
	}
}

// header copies k (≤16) bytes outside any record's length.
func (h *hprof) header(k int) ([]byte, error) {
	b := h.buf[:k]
	if _, err := io.ReadFull(h.r, b); err != nil {
		return nil, err
	}
	_, err := h.w.Write(b)
	return b, err
}

func (h *hprof) heapDump() error {
	id := int64(h.id)
	for h.left > 0 {
		t, err := h.take(1)
		if err != nil {
			return err
		}
		switch t[0] {
		case 0xFF, 0x05, 0x07: // root unknown, sticky class, monitor used
			err = h.copy(id)
		case 0x01: // root JNI global
			err = h.copy(2 * id)
		case 0x04, 0x06: // root native stack, thread block
			err = h.copy(id + 4)
		case 0x02, 0x03, 0x08: // root JNI local, java frame, thread object
			err = h.copy(id + 8)
		case 0x20:
			err = h.classDump()
		case 0x21: // instance: id, u4 stack, id class, u4 n, n bytes
			if err = h.copy(2*id + 4); err == nil {
				var n uint32
				if n, err = h.u4(); err == nil {
					err = h.copy(int64(n))
				}
			}
		case 0x22: // object array: id, u4 stack, u4 n, id class, n ids
			if err = h.copy(id + 4); err == nil {
				var n uint32
				if n, err = h.u4(); err == nil {
					err = h.copy(id + int64(n)*id)
				}
			}
		case 0x23: // primitive array: id, u4 stack, u4 n, u1 type, n values
			if err = h.copy(id + 4); err == nil {
				var n uint32
				if n, err = h.u4(); err == nil {
					var size int
					if size, err = h.typeSize(); err == nil {
						err = h.zero(int64(n) * int64(size))
					}
				}
			}
		default:
			return fmt.Errorf("hprof: unknown heap dump sub-record 0x%02x", t[0])
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// classDump copies a class dump: ids and sizes, then constant pool, static and
// instance field tables.
func (h *hprof) classDump() error {
	id := int64(h.id)
	if err := h.copy(7*id + 8); err != nil {
		return err
	}
	for _, head := range []int64{2, id} { // constant pool: u2 index; statics: id name
		n, err := h.u2()
		if err != nil {
			return err
		}
		for range n {
			if err := h.copy(head); err != nil {
				return err
			}
			size, err := h.typeSize()
			if err != nil {
				return err
			}
			if err := h.copy(int64(size)); err != nil {
				return err
			}
		}
	}
	n, err := h.u2()
	if err != nil {
		return err
	}
	return h.copy(int64(n) * (id + 1)) // instance fields: id name, u1 type
}

// typeSize copies a u1 basic type and returns the size of one value of it.
func (h *hprof) typeSize() (int, error) {
	t, err := h.take(1)
	if err != nil {
		return 0, err
	}
	switch t[0] {
	case 2:
		return h.id, nil
	case 4, 8:
		return 1, nil
	case 5, 9:
		return 2, nil
	case 6, 10:
		return 4, nil
	case 7, 11:
		return 8, nil
	}
	return 0, fmt.Errorf("hprof: unknown basic type %d", t[0])
}

func (h *hprof) u2() (uint16, error) {
	b, err := h.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (h *hprof) u4() (uint32, error) {
	b, err := h.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (h *hprof) claim(k int64) error {
	if k > h.left {
		return errors.New("hprof: record overruns its length")
	}
	h.left -= k
	return nil
}

// take copies k (≤16) bytes of the current record through and returns them.
func (h *hprof) take(k int) ([]byte, error) {
	if err := h.claim(int64(k)); err != nil {
		return nil, err
	}
	b, err := h.header(k)
	return b, unexpected(err)
}

func (h *hprof) copy(k int64) error {
	if err := h.claim(k); err != nil {
		return err
	}
	_, err := io.CopyN(h.w, h.r, k)
	return unexpected(err)
}

var zeros [32 << 10]byte

func (h *hprof) zero(k int64) error {
	if err := h.claim(k); err != nil {
		return err
	}
	for k > 0 {
		c := min(k, int64(len(zeros)))
		if _, err := h.r.Discard(int(c)); err != nil {
			return unexpected(err)
		}
		if _, err := h.w.Write(zeros[:c]); err != nil {
			return err
		}
		k -= c
	}
	return nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Bundle rewrites one gzipped tar member from r to w, scrubbing each .hprof
// entry with HPROF. Other entries pass through unchanged.
func Bundle(r io.Reader, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	gz.Multistream(false)
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if strings.HasSuffix(hdr.Name, ".hprof") {
			err = HPROF(tr, tw)
		} else {
			_, err = io.Copy(tw, tr)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil { // validate the gzip footer
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}
