package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// receivedSTTMessage is one decoded input_audio_chunk received by the fake STT server.
type receivedSTTMessage struct {
	audioData []byte
	commit    bool
}

// startFakeSTTServer starts a WebSocket server that decodes every audio chunk it
// receives and forwards it to the returned channel in arrival order. The handler waits
// for release before upgrading, so a test can queue audio while the worker's dial is
// still blocked and prove that nothing is written before the handshake completes.
func startFakeSTTServer(t *testing.T, release <-chan struct{}) (*httptest.Server, chan receivedSTTMessage) {
	t.Helper()

	messages := make(chan receivedSTTMessage, 64)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-release
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var message struct {
				AudioBase64 string `json:"audio_base_64"`
				Commit      bool   `json:"commit"`
			}
			if err := json.Unmarshal(data, &message); err != nil {
				return
			}
			audioData, err := base64.StdEncoding.DecodeString(message.AudioBase64)
			if err != nil {
				return
			}
			messages <- receivedSTTMessage{audioData: audioData, commit: message.Commit}
		}
	}))
	t.Cleanup(server.Close)
	return server, messages
}

// TestSTTWorker_forwardsAudioInOrderThenCommit verifies that chunks queued while the
// worker is still dialing reach ElevenLabs in the same order, followed by the commit.
func TestSTTWorker_forwardsAudioInOrderThenCommit(t *testing.T) {
	release := make(chan struct{})
	server, messages := startFakeSTTServer(t, release)
	sttURL := "ws" + strings.TrimPrefix(server.URL, "http")

	worker := startSTTWorker(sttURL, "test-key", make(chan string, 1))
	defer worker.stop()

	chunks := [][]byte{{0x01}, {0x02, 0x03}, {0x04, 0x05, 0x06}}
	for _, chunk := range chunks {
		worker.enqueue(sttAudioItem{audioData: chunk})
	}
	worker.enqueue(sttAudioItem{commit: true})

	// The server handler is still blocked on release, so nothing can have been written
	// yet. Failing here but continuing is safe: release is closed below regardless.
	select {
	case received := <-messages:
		t.Errorf("server received a message before the WebSocket handshake completed: %+v", received)
	default:
	}

	// Release the handshake only after every chunk and the commit are queued, so the
	// worker can only drain the queue once it has connected.
	close(release)

	for index, want := range chunks {
		select {
		case received := <-messages:
			if !bytes.Equal(received.audioData, want) {
				t.Fatalf("message %d audio: got %x, want %x", index, received.audioData, want)
			}
			if received.commit {
				t.Fatalf("message %d unexpectedly has commit set", index)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for audio message %d", index)
		}
	}

	select {
	case received := <-messages:
		if !received.commit {
			t.Error("final message should be the commit")
		}
		if len(received.audioData) != 0 {
			t.Errorf("commit message should carry no audio, got %d bytes", len(received.audioData))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for commit")
	}
}

// TestSTTWorker_dialFailureClosesTranscriptChannel verifies that a failed dial closes the
// transcript channel immediately so the run fails fast instead of waiting out the timeout.
func TestSTTWorker_dialFailureClosesTranscriptChannel(t *testing.T) {
	// Bind a port and close it so the dial is refused deterministically.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()

	transcriptChannel := make(chan string, 1)
	worker := startSTTWorker("ws://"+address, "test-key", transcriptChannel)
	defer worker.stop()

	select {
	case _, ok := <-transcriptChannel:
		if ok {
			t.Error("transcript channel should be closed, not carry a transcript")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transcript channel was not closed after the dial failed")
	}
}
