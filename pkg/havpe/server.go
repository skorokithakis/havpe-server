package havpe

import (
	"log"
	"net/http"
	"sync"

	"github.com/skorokithakis/havpe-server/pkg/elevenlabs"
	"github.com/skorokithakis/havpe-server/pkg/processors"

	"github.com/streamer45/silero-vad-go/speech"
)

// Settings holds runtime-configurable values that can be loaded from a JSON file
// and overridden by environment variables.
type Settings struct {
	SttLanguage string  `json:"stt_language"`
	TtsSpeed    float64 `json:"tts_speed"`
}

// settingsUpdate is used to decode a PUT /settings request body. Pointer fields
// allow partial updates.
type settingsUpdate struct {
	SttLanguage *string  `json:"stt_language"`
	TtsSpeed    *float64 `json:"tts_speed"`
}

// Server is the main application server. It implements esphome.VoiceHandler and
// owns the voice pipeline, HTTP API, and all runtime state. Transcript processing
// is delegated to a chain of TranscriptProcessors.
type Server struct {
	sttClient   *elevenlabs.STTClient
	ttsClient   *elevenlabs.TTSClient
	audioBuffer *elevenlabs.AudioBuffer
	detector    *speech.Detector

	processors []processors.TranscriptProcessor

	apiPassword string
	recordDir   string

	settings     Settings
	settingsMu   sync.RWMutex
	settingsPath string

	ToneURL        string
	ErrorURL       string
	TTSResponseURL string

	pipeline         pipelineState
	recordingCounter int
}

// ServerConfig holds the parameters for NewServer.
type ServerConfig struct {
	STTClient   *elevenlabs.STTClient
	TTSClient   *elevenlabs.TTSClient
	AudioBuffer *elevenlabs.AudioBuffer
	Detector    *speech.Detector
	Processors  []processors.TranscriptProcessor
	APIPassword string
	RecordDir   string
	Settings    Settings
	SettingsPath string
}

// NewServer creates a new Server with the given configuration.
func NewServer(cfg ServerConfig) *Server {
	return &Server{
		sttClient:   cfg.STTClient,
		ttsClient:   cfg.TTSClient,
		audioBuffer: cfg.AudioBuffer,
		detector:    cfg.Detector,
		processors:  cfg.Processors,
		apiPassword: cfg.APIPassword,
		recordDir:   cfg.RecordDir,
		settings:    cfg.Settings,
		settingsPath: cfg.SettingsPath,
	}
}

// RegisterRoutes registers the core HTTP handlers on the given mux. If mux is
// nil, http.DefaultServeMux is used. Processor-specific routes (e.g. shortcuts
// CRUD) should be registered separately by the processor.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	if mux == nil {
		mux = http.DefaultServeMux
	}

	mux.HandleFunc("/settings", s.handleSettings)
	log.Printf("settings API enabled at /settings")

	mux.HandleFunc("/tone.wav", s.handleToneWav)
	mux.HandleFunc("/error.wav", s.handleErrorWav)
	mux.HandleFunc("/tts.mp3", s.handleTTSMP3)
}
