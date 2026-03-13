package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// buildFrame manually constructs a raw plaintext ESPHome frame for use in tests.
func buildFrame(messageType uint32, body []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(plaintextPreamble)
	varintBuf := make([]byte, binary.MaxVarintLen64)
	buf.Write(varintBuf[:binary.PutUvarint(varintBuf, uint64(len(body)))])
	buf.Write(varintBuf[:binary.PutUvarint(varintBuf, uint64(messageType))])
	buf.Write(body)
	return buf.Bytes()
}

func TestReadFrame_roundtrip(t *testing.T) {
	body := []byte{0x0a, 0x05, 0x68, 0x65, 0x6c, 0x6c, 0x6f}
	raw := buildFrame(42, body)

	gotType, gotData, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotType != 42 {
		t.Errorf("message type: got %d, want 42", gotType)
	}
	if !bytes.Equal(gotData, body) {
		t.Errorf("body: got %x, want %x", gotData, body)
	}
}

func TestReadFrame_emptyBody(t *testing.T) {
	raw := buildFrame(1, []byte{})

	gotType, gotData, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotType != 1 {
		t.Errorf("message type: got %d, want 1", gotType)
	}
	if len(gotData) != 0 {
		t.Errorf("expected empty body, got %x", gotData)
	}
}

func TestReadFrame_badPreamble(t *testing.T) {
	// 0x01 is the noise/encrypted preamble — must be rejected.
	raw := []byte{0x01, 0x00, 0x01}
	_, _, err := ReadFrame(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error for bad preamble, got nil")
	}
}

func TestReadFrame_truncatedAfterPreamble(t *testing.T) {
	_, _, err := ReadFrame(bytes.NewReader([]byte{0x00}))
	if err == nil {
		t.Fatal("expected error for truncated frame, got nil")
	}
}

func TestReadFrame_truncatedBody(t *testing.T) {
	// Claim body length 5 but only provide 2 bytes.
	var buf bytes.Buffer
	buf.WriteByte(plaintextPreamble)
	varintBuf := make([]byte, binary.MaxVarintLen64)
	buf.Write(varintBuf[:binary.PutUvarint(varintBuf, 5)])
	buf.Write(varintBuf[:binary.PutUvarint(varintBuf, 1)])
	buf.Write([]byte{0xAA, 0xBB}) // only 2 of the promised 5 bytes

	_, _, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error for truncated body, got nil")
	}
}

func TestWriteFrame_roundtrip(t *testing.T) {
	body := []byte{0x01, 0x02, 0x03}
	var buf bytes.Buffer

	if err := WriteFrame(&buf, 7, body); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotType, gotData, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if gotType != 7 {
		t.Errorf("message type: got %d, want 7", gotType)
	}
	if !bytes.Equal(gotData, body) {
		t.Errorf("body: got %x, want %x", gotData, body)
	}
}

func TestWriteFrame_emptyBody(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, 3, []byte{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotType, gotData, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if gotType != 3 {
		t.Errorf("message type: got %d, want 3", gotType)
	}
	if len(gotData) != 0 {
		t.Errorf("expected empty body, got %x", gotData)
	}
}

func TestWriteFrame_largeMessageType(t *testing.T) {
	// Message types above 127 require multi-byte varints.
	body := []byte{0xFF}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, 300, body); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	gotType, gotData, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("unexpected read error: %v", err)
	}
	if gotType != 300 {
		t.Errorf("message type: got %d, want 300", gotType)
	}
	if !bytes.Equal(gotData, body) {
		t.Errorf("body: got %x, want %x", gotData, body)
	}
}

func TestWriteFrame_writeError(t *testing.T) {
	err := WriteFrame(errorWriter{}, 1, []byte{0x01})
	if err == nil {
		t.Fatal("expected error from failing writer, got nil")
	}
}

// errorWriter always returns an error on Write.
type errorWriter struct{}

func (errorWriter) Write(_ []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestReadFrame_multipleFrames(t *testing.T) {
	// Verify that sequential frames on the same reader are decoded independently.
	var buf bytes.Buffer
	frames := []struct {
		messageType uint32
		body        []byte
	}{
		{1, []byte{0xAA}},
		{2, []byte{0xBB, 0xCC}},
		{300, []byte{}},
	}
	for _, f := range frames {
		if err := WriteFrame(&buf, f.messageType, f.body); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	for _, want := range frames {
		gotType, gotData, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if gotType != want.messageType {
			t.Errorf("message type: got %d, want %d", gotType, want.messageType)
		}
		if !bytes.Equal(gotData, want.body) {
			t.Errorf("body: got %x, want %x", gotData, want.body)
		}
	}
}
