package processors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// WebhookProcessor sends the transcript to an external webhook and returns
// the response text for TTS synthesis.
type WebhookProcessor struct {
	URL     string
	Payload string
}

// NewWebhookProcessor creates a WebhookProcessor. payload is a JSON template
// where the literal string "$transcript" is replaced with the transcript text.
func NewWebhookProcessor(url, payload string) *WebhookProcessor {
	return &WebhookProcessor{URL: url, Payload: payload}
}

// Process implements TranscriptProcessor. It sends the transcript to the
// webhook, sets the response text from the reply, and stops the chain.
func (w *WebhookProcessor) Process(req *TranscriptRequest, resp *TranscriptResponse) error {
	escapedBytes, err := json.Marshal(req.Transcript)
	if err != nil {
		return fmt.Errorf("JSON-escape transcript: %w", err)
	}
	escaped := string(escapedBytes[1 : len(escapedBytes)-1])
	payloadBody := strings.Replace(w.Payload, "$transcript", escaped, 1)

	response, err := http.Post(w.URL, "application/json", bytes.NewReader([]byte(payloadBody)))
	if err != nil {
		return fmt.Errorf("webhook POST: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("webhook returned status %d: %s", response.StatusCode, body)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read webhook response body: %w", err)
	}
	log.Printf("webhook response: %s", body)

	var result struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("webhook response is not JSON, treating as empty response: %v", err)
	} else {
		resp.ResponseText = result.Response
	}
	resp.StopProcessing = true
	return nil
}
