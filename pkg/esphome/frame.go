package esphome

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

const PlaintextPreamble = 0x00

// ReadFrame reads one plaintext ESPHome native API frame from reader.
// The frame format is: preamble (0x00) | size varint | type varint | body bytes.
// Returns the message type ID and the raw protobuf body bytes.
func ReadFrame(reader io.Reader) (messageType uint32, data []byte, err error) {
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
	if preamble != PlaintextPreamble {
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
// The entire frame is assembled into a single slice before calling writer.Write
// once, which is essential when writer is a LockedWriter to prevent interleaving.
func WriteFrame(writer io.Writer, messageType uint32, data []byte) error {
	frame := make([]byte, 1+binary.MaxVarintLen64+binary.MaxVarintLen64+len(data))
	frame[0] = PlaintextPreamble
	offset := 1
	offset += binary.PutUvarint(frame[offset:], uint64(len(data)))
	offset += binary.PutUvarint(frame[offset:], uint64(messageType))
	copy(frame[offset:], data)
	offset += len(data)

	if _, err := writer.Write(frame[:offset]); err != nil {
		return fmt.Errorf("writing frame: %w", err)
	}
	return nil
}
