package elevenlabs

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
)

// STTClient wraps the ElevenLabs realtime Speech-to-Text WebSocket API.
type STTClient struct {
	APIKey string
}

func NewSTTClient(apiKey string) *STTClient {
	return &STTClient{APIKey: apiKey}
}

// Dial opens a WebSocket connection to the ElevenLabs realtime STT API for the
// given language. The caller is responsible for closing the returned connection.
func (c *STTClient) Dial(language string) (*websocket.Conn, error) {
	sttURL := "wss://api.elevenlabs.io/v1/speech-to-text/realtime?model_id=scribe_v2_realtime&language_code=" +
		url.QueryEscape(language) + "&audio_format=pcm_16000&commit_strategy=manual"
	dialer := websocket.Dialer{}
	headers := http.Header{}
	headers.Set("xi-api-key", c.APIKey)
	conn, _, err := dialer.Dial(sttURL, headers)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// ReadSTTMessages reads messages from the ElevenLabs realtime STT WebSocket until
// the connection closes. When a committed_transcript message arrives, its text is
// sent to transcriptChannel and the function returns. If the connection drops before
// a transcript arrives, the channel is closed.
func ReadSTTMessages(conn *websocket.Conn, transcriptChannel chan string) {
	lastPartialText := ""
	for {
		_, messageBytes, err := conn.ReadMessage()
		if err != nil {
			log.Printf("STT WebSocket read: %v", err)
			close(transcriptChannel)
			return
		}

		var message struct {
			MessageType string `json:"message_type"`
			Text        string `json:"text"`
			Error       string `json:"error"`
		}
		if err := json.Unmarshal(messageBytes, &message); err != nil {
			log.Printf("STT WebSocket: unmarshal message: %v", err)
			continue
		}

		if message.Error != "" {
			log.Printf("STT WebSocket: error from server: type=%q error=%q", message.MessageType, message.Error)
		}

		switch message.MessageType {
		case "committed_transcript":
			text := message.Text
			if text == "" {
				text = lastPartialText
				log.Printf("STT WebSocket: committed_transcript empty, falling back to last partial: %q", text)
			} else {
				log.Printf("STT WebSocket: committed_transcript: %q", text)
			}
			select {
			case transcriptChannel <- text:
			default:
				log.Printf("STT WebSocket: transcriptChannel full, discarding duplicate transcript")
			}
			return
		case "partial_transcript":
			if message.Text != "" {
				lastPartialText = message.Text
			}
			log.Printf("STT WebSocket: received message: %s", messageBytes)
		case "session_started":
			log.Printf("STT WebSocket: session started")
		default:
			log.Printf("STT WebSocket: received message: %s", messageBytes)
		}
	}
}

// SendSTTChunk sends a single PCM audio chunk to the ElevenLabs realtime STT
// WebSocket. When commit is true, the message signals ElevenLabs to finalize
// transcription. Returns true on success, false on any error.
func SendSTTChunk(conn *websocket.Conn, audioData []byte, commit bool) bool {
	message := struct {
		MessageType string `json:"message_type"`
		AudioBase64 string `json:"audio_base_64"`
		Commit      bool   `json:"commit"`
		SampleRate  int    `json:"sample_rate"`
	}{
		MessageType: "input_audio_chunk",
		AudioBase64: base64.StdEncoding.EncodeToString(audioData),
		Commit:      commit,
		SampleRate:  16000,
	}
	messageBytes, err := json.Marshal(message)
	if err != nil {
		log.Printf("sendSTTChunk: marshal message: %v", err)
		return false
	}
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, messageBytes); err != nil {
		log.Printf("sendSTTChunk: write message: %v", err)
		return false
	}
	return true
}
