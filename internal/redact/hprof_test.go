package redact

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestHPROF(t *testing.T) {
	be := binary.BigEndian
	var seg bytes.Buffer
	seg.WriteByte(0x23) // primitive byte[]
	_ = binary.Write(&seg, be, uint32(1))
	_ = binary.Write(&seg, be, uint32(0))
	_ = binary.Write(&seg, be, uint32(7))
	seg.WriteByte(8)
	seg.WriteString("hunter2")
	seg.WriteByte(0x21) // instance with 4 field bytes
	_ = binary.Write(&seg, be, []uint32{2, 0, 3, 4, 0xCAFEBABE})
	seg.WriteByte(0x20) // class dump: 1 static int, 1 instance field
	_ = binary.Write(&seg, be, []uint32{3, 0, 0, 0, 0, 0, 0, 0, 16})
	_ = binary.Write(&seg, be, uint16(0))
	_ = binary.Write(&seg, be, uint16(1))
	_ = binary.Write(&seg, be, uint32(9))
	seg.WriteByte(10)
	_ = binary.Write(&seg, be, uint32(42))
	_ = binary.Write(&seg, be, uint16(1))
	_ = binary.Write(&seg, be, uint32(9))
	seg.WriteByte(2)

	var in bytes.Buffer
	in.WriteString("JAVA PROFILE 1.0.2\x00")
	_ = binary.Write(&in, be, uint32(4))
	_ = binary.Write(&in, be, uint64(0))
	in.WriteByte(0x01) // UTF8 record
	_ = binary.Write(&in, be, []uint32{0, 8, 9})
	in.WriteString("name")
	in.WriteByte(0x1C)
	_ = binary.Write(&in, be, []uint32{0, uint32(seg.Len())})
	in.Write(seg.Bytes())

	var out bytes.Buffer
	if err := HPROF(bytes.NewReader(in.Bytes()), &out); err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(in.Bytes(), []byte("hunter2"), make([]byte, 7), 1)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("got %q\nwant %q", out.Bytes(), want)
	}

	if err := HPROF(bytes.NewReader(in.Bytes()[:in.Len()-3]), &bytes.Buffer{}); err == nil {
		t.Error("truncated dump must fail, not pass through")
	}
}
