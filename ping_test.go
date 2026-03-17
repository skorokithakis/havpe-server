package main

import (
	"bufio"
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"havpe-server/api"
)

// writeCapture is a net.Conn whose Write method appends to buf under mu.
// It is used to capture writes from lockedWriter in tests without a real network connection.
type writeCapture struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (wc *writeCapture) Write(p []byte) (int, error) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	return wc.buf.Write(p)
}

func (wc *writeCapture) Read(_ []byte) (int, error)         { return 0, nil }
func (wc *writeCapture) Close() error                       { return nil }
func (wc *writeCapture) LocalAddr() net.Addr                { return nil }
func (wc *writeCapture) RemoteAddr() net.Addr               { return nil }
func (wc *writeCapture) SetDeadline(_ time.Time) error      { return nil }
func (wc *writeCapture) SetReadDeadline(_ time.Time) error  { return nil }
func (wc *writeCapture) SetWriteDeadline(_ time.Time) error { return nil }

// TestWriteFrame_concurrentFramesDoNotCorrupt verifies that calling WriteFrame
// concurrently from multiple goroutines through a lockedWriter produces a stream
// where every frame can be read back without corruption. This is the key property
// that the single-Write-call implementation of WriteFrame must guarantee: if the
// header and body were written in two separate Write calls, the lock would be
// released between them and a concurrent goroutine could interleave its bytes.
//
// Run with -race to also catch any data races on the underlying buffer.
func TestWriteFrame_concurrentFramesDoNotCorrupt(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer

	lw := &lockedWriter{conn: &writeCapture{mu: &mu, buf: &buf}}

	const goroutines = 10
	const framesPerGoroutine = 50

	// Each goroutine writes frames whose body is a single repeated byte equal to
	// the goroutine index. This makes it easy to verify that no frame's body was
	// partially overwritten by another goroutine's bytes.
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for index := range goroutines {
		index := index
		go func() {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(index)}, 8)
			for range framesPerGoroutine {
				if err := WriteFrame(lw, messageTypePingRequest, body); err != nil {
					t.Errorf("WriteFrame (goroutine %d): %v", index, err)
				}
			}
		}()
	}
	wg.Wait()

	// Read all frames back and verify each one is well-formed: correct message type
	// and a body consisting entirely of a single repeated byte value.
	mu.Lock()
	captured := make([]byte, buf.Len())
	copy(captured, buf.Bytes())
	mu.Unlock()

	reader := bufio.NewReader(bytes.NewReader(captured))
	totalFrames := goroutines * framesPerGoroutine
	for frameIndex := range totalFrames {
		messageType, body, err := ReadFrame(reader)
		if err != nil {
			t.Fatalf("ReadFrame (frame %d): %v", frameIndex, err)
		}
		if messageType != messageTypePingRequest {
			t.Errorf("frame %d: expected message type %d, got %d", frameIndex, messageTypePingRequest, messageType)
		}
		if len(body) != 8 {
			t.Errorf("frame %d: expected body length 8, got %d", frameIndex, len(body))
			continue
		}
		// All bytes in the body must be the same value — any mix indicates interleaving.
		first := body[0]
		for byteIndex, b := range body {
			if b != first {
				t.Errorf("frame %d: body byte %d is 0x%02x, want 0x%02x (frame body was corrupted by interleaving)",
					frameIndex, byteIndex, b, first)
				break
			}
		}
	}
}

// TestPingGoroutine_sendsPingRequest verifies that the ping goroutine sends at least
// one PingRequest frame within a short interval when given a fast ticker.
// This test exercises the goroutine's select loop and frame encoding path.
func TestPingGoroutine_sendsPingRequest(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer

	lw := &lockedWriter{conn: &writeCapture{mu: &mu, buf: &buf}}

	done := make(chan struct{})

	// Use a very short interval so the test completes quickly.
	const testInterval = 5 * time.Millisecond

	go func() {
		ticker := time.NewTicker(testInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := sendMessage(lw, messageTypePingRequest, &api.PingRequest{}); err != nil {
					return
				}
			}
		}
	}()

	// Wait long enough for at least two ticks.
	time.Sleep(testInterval * 5)
	close(done)
	// Give the goroutine a moment to exit before reading the buffer.
	time.Sleep(testInterval)

	mu.Lock()
	data := make([]byte, buf.Len())
	copy(data, buf.Bytes())
	mu.Unlock()

	if len(data) == 0 {
		t.Fatal("expected at least one PingRequest frame to be written, got none")
	}

	// Verify the first frame is a valid PingRequest (message type 7, empty body).
	reader := bufio.NewReader(bytes.NewReader(data))
	messageType, body, err := ReadFrame(reader)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if messageType != messageTypePingRequest {
		t.Errorf("expected message type %d (PingRequest), got %d", messageTypePingRequest, messageType)
	}
	if len(body) != 0 {
		t.Errorf("expected empty PingRequest body, got %d bytes", len(body))
	}
}
