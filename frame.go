package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

const plaintextPreamble = 0x00

// ReadFrame reads one plaintext ESPHome native API frame from reader.
// The frame format is: preamble (0x00) | size varint | type varint | body bytes.
// Returns the message type ID and the raw protobuf body bytes.
func ReadFrame(reader io.Reader) (messageType uint32, data []byte, err error) {
	// encoding/binary.ReadUvarint requires an io.ByteReader. Wrap in bufio.Reader
	// only when necessary so callers that already pass a bufio.Reader don't pay
	// double-buffering costs. We keep a reference to the io.Reader side of the
	// (possibly wrapped) reader so that io.ReadFull for the body drains from the
	// same buffer as the varint reads above.
	fullReader := reader
	byteReader, ok := reader.(io.ByteReader)
	if !ok {
		br := bufio.NewReader(reader)
		byteReader = br
		fullReader = br
	}

	preamble, err := byteReader.ReadByte()
	if err != nil {
		return 0, nil, fmt.Errorf("reading preamble: %w", err)
	}
	if preamble != plaintextPreamble {
		return 0, nil, fmt.Errorf("unexpected preamble byte 0x%02x (noise/encrypted frames are not supported)", preamble)
	}

	size, err := binary.ReadUvarint(byteReader)
	if err != nil {
		return 0, nil, fmt.Errorf("reading size varint: %w", err)
	}

	rawType, err := binary.ReadUvarint(byteReader)
	if err != nil {
		return 0, nil, fmt.Errorf("reading type varint: %w", err)
	}

	body := make([]byte, size)
	if _, err = io.ReadFull(fullReader, body); err != nil {
		return 0, nil, fmt.Errorf("reading body (%d bytes): %w", size, err)
	}

	return uint32(rawType), body, nil
}

// WriteFrame writes one plaintext ESPHome native API frame to writer.
// The frame format is: preamble (0x00) | size varint | type varint | body bytes.
func WriteFrame(writer io.Writer, messageType uint32, data []byte) error {
	// Pre-allocate a buffer large enough for the worst case: preamble (1) +
	// two varints (up to 10 bytes each) + body.
	header := make([]byte, 1+binary.MaxVarintLen64+binary.MaxVarintLen64)
	header[0] = plaintextPreamble
	offset := 1
	offset += binary.PutUvarint(header[offset:], uint64(len(data)))
	offset += binary.PutUvarint(header[offset:], uint64(messageType))

	if _, err := writer.Write(header[:offset]); err != nil {
		return fmt.Errorf("writing frame header: %w", err)
	}
	if len(data) > 0 {
		if _, err := writer.Write(data); err != nil {
			return fmt.Errorf("writing frame body: %w", err)
		}
	}
	return nil
}
