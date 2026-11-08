package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// trackedReadCloser records whether it was closed so tests can assert that the handler
// releases the upstream response instead of leaking it.
type trackedReadCloser struct {
	io.Reader
	closed atomic.Bool
}

func (reader *trackedReadCloser) Close() error {
	reader.closed.Store(true)
	return nil
}

// gatedReader yields its first chunk immediately and blocks the rest until release is
// closed, so a test can tell whether the handler forwarded audio before the upstream
// body finished.
type gatedReader struct {
	first     []byte
	rest      []byte
	release   chan struct{}
	delivered bool
}

func (reader *gatedReader) Read(buffer []byte) (int, error) {
	if !reader.delivered {
		reader.delivered = true
		return copy(buffer, reader.first), nil
	}
	<-reader.release
	if len(reader.rest) == 0 {
		return 0, io.EOF
	}
	read := copy(buffer, reader.rest)
	reader.rest = reader.rest[read:]
	return read, nil
}

// TestHandleTTSAudio_streamsBeforeUpstreamFinishes verifies that the handler forwards
// each chunk as it arrives instead of buffering the whole upstream body, and that the
// stream is one-shot: the handler closes it, and a second fetch gets a 404.
func TestHandleTTSAudio_streamsBeforeUpstreamFinishes(t *testing.T) {
	t.Cleanup(func() {
		if stream := takePendingTTSStream(); stream != nil {
			stream.Close()
		}
	})

	release := make(chan struct{})
	upstream := &trackedReadCloser{Reader: &gatedReader{
		first:   []byte("first"),
		rest:    []byte("second"),
		release: release,
	}}
	setPendingTTSStream(upstream)

	server := httptest.NewServer(http.HandlerFunc(handleTTSAudio))
	defer server.Close()

	response, err := server.Client().Get(server.URL + "/tts.mp3")
	if err != nil {
		t.Fatalf("GET /tts.mp3: %v", err)
	}
	defer response.Body.Close()

	if got := response.Header.Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type: got %q, want %q", got, "audio/mpeg")
	}

	// The first chunk must arrive while the upstream is still blocked; if the handler
	// buffered the body, this read would not complete until release is closed.
	firstChunk := make([]byte, len("first"))
	firstRead := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(response.Body, firstChunk)
		firstRead <- err
	}()
	select {
	case err := <-firstRead:
		if err != nil {
			close(release)
			t.Fatalf("read first chunk: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("handler did not forward the first chunk before the upstream body finished")
	}
	if string(firstChunk) != "first" {
		t.Fatalf("first chunk: got %q, want %q", firstChunk, "first")
	}

	close(release)
	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read remaining body: %v", err)
	}
	if string(rest) != "second" {
		t.Errorf("remaining body: got %q, want %q", rest, "second")
	}
	if !upstream.closed.Load() {
		t.Error("handler should close the TTS stream when the transfer ends")
	}

	second, err := server.Client().Get(server.URL + "/tts.mp3")
	if err != nil {
		t.Fatalf("second GET /tts.mp3: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Errorf("second fetch status: got %d, want %d", second.StatusCode, http.StatusNotFound)
	}
}

// TestSetPendingTTSStream_closesReplacedStream verifies that opening a new TTS stream
// closes one the device never fetched, so abandoned response bodies do not leak.
func TestSetPendingTTSStream_closesReplacedStream(t *testing.T) {
	t.Cleanup(func() {
		if stream := takePendingTTSStream(); stream != nil {
			stream.Close()
		}
	})

	unread := &trackedReadCloser{Reader: strings.NewReader("old")}
	setPendingTTSStream(unread)
	setPendingTTSStream(io.NopCloser(strings.NewReader("new")))

	if !unread.closed.Load() {
		t.Error("replacing an unread TTS stream should close it")
	}
}
