package elevenlabs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
)

// TTSClient wraps the ElevenLabs Text-to-Speech REST API.
type TTSClient struct {
	APIKey string
}

func NewTTSClient(apiKey string) *TTSClient {
	return &TTSClient{APIKey: apiKey}
}

// SynthesizeSpeech calls the ElevenLabs TTS API and returns the MP3 audio bytes.
func (c *TTSClient) SynthesizeSpeech(text string, speed float64) ([]byte, error) {
	payload := map[string]interface{}{
		"text":     text,
		"model_id": "eleven_v3",
		"voice_settings": map[string]interface{}{
			"speed": speed,
		},
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal TTS payload: %w", err)
	}
	log.Printf("TTS request payload: %s", payloadBytes)

	request, err := http.NewRequest("POST", "https://api.elevenlabs.io/v1/text-to-speech/bIHbv24MWmeRgasZH58o", bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("create TTS request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("xi-api-key", c.APIKey)
	request.Header.Set("Accept", "audio/mpeg")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("TTS request: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read TTS response body: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TTS API returned status %d: %s", response.StatusCode, responseBody)
	}

	return responseBody, nil
}

// AudioBuffer is a mutex-protected byte buffer for serving the most recently
// synthesized TTS audio. It is shared between the pipeline (writes) and the
// HTTP handler for /tts.mp3 (reads).
type AudioBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *AudioBuffer) Set(data []byte) {
	b.mu.Lock()
	b.data = data
	b.mu.Unlock()
}

func (b *AudioBuffer) Get() []byte {
	b.mu.Lock()
	result := make([]byte, len(b.data))
	copy(result, b.data)
	b.mu.Unlock()
	return result
}
