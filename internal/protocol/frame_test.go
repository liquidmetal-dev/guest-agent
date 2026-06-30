package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		typ     FrameType
		payload []byte
	}{
		{FrameRequest, []byte(`{"op":"exec"}`)},
		{FrameStdout, []byte("hello world")},
		{FrameStdinEOF, nil},
		{FrameExit, []byte(`{"code":0}`)},
	}
	var buf bytes.Buffer
	for _, c := range cases {
		if err := WriteFrame(&buf, c.typ, c.payload); err != nil {
			t.Fatalf("WriteFrame %s: %v", c.typ, err)
		}
	}
	for _, c := range cases {
		f, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame %s: %v", c.typ, err)
		}
		if f.Type != c.typ {
			t.Errorf("type = %s, want %s", f.Type, c.typ)
		}
		if !bytes.Equal(f.Payload, c.payload) && !(len(f.Payload) == 0 && len(c.payload) == 0) {
			t.Errorf("payload = %q, want %q", f.Payload, c.payload)
		}
	}
}

func TestReadFrameCleanEOF(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func TestWriteFrameTooLarge(t *testing.T) {
	var buf bytes.Buffer
	big := make([]byte, MaxFrameSize+1)
	if err := WriteFrame(&buf, FrameStdout, big); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameTooLarge(t *testing.T) {
	var hdr [headerSize]byte
	hdr[0] = byte(FrameStdout)
	binary.BigEndian.PutUint32(hdr[1:], MaxFrameSize+1)
	if _, err := ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}
