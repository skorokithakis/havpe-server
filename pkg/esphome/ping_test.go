package esphome

import (
	"bufio"
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/skorokithakis/havpe-server/pkg/esphome/api"
)

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

func TestWriteFrame_concurrentFramesDoNotCorrupt(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer

	lw := &LockedWriter{Conn: &writeCapture{mu: &mu, buf: &buf}}

	const goroutines = 10
	const framesPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for index := range goroutines {
		index := index
		go func() {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(index)}, 8)
			for range framesPerGoroutine {
				if err := WriteFrame(lw, MessageTypePingRequest, body); err != nil {
					t.Errorf("WriteFrame (goroutine %d): %v", index, err)
				}
			}
		}()
	}
	wg.Wait()

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
		if messageType != MessageTypePingRequest {
			t.Errorf("frame %d: expected message type %d, got %d", frameIndex, MessageTypePingRequest, messageType)
		}
		if len(body) != 8 {
			t.Errorf("frame %d: expected body length 8, got %d", frameIndex, len(body))
			continue
		}
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

func TestPingGoroutine_sendsPingRequest(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer

	lw := &LockedWriter{Conn: &writeCapture{mu: &mu, buf: &buf}}

	done := make(chan struct{})

	const testInterval = 5 * time.Millisecond

	go func() {
		ticker := time.NewTicker(testInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := SendMessage(lw, MessageTypePingRequest, &api.PingRequest{}); err != nil {
					return
				}
			}
		}
	}()

	time.Sleep(testInterval * 5)
	close(done)
	time.Sleep(testInterval)

	mu.Lock()
	data := make([]byte, buf.Len())
	copy(data, buf.Bytes())
	mu.Unlock()

	if len(data) == 0 {
		t.Fatal("expected at least one PingRequest frame to be written, got none")
	}

	reader := bufio.NewReader(bytes.NewReader(data))
	messageType, body, err := ReadFrame(reader)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if messageType != MessageTypePingRequest {
		t.Errorf("expected message type %d (PingRequest), got %d", MessageTypePingRequest, messageType)
	}
	if len(body) != 0 {
		t.Errorf("expected empty PingRequest body, got %d bytes", len(body))
	}
}
