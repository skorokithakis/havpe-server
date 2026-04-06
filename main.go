package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"havpe-server/api"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/mdns"
	"github.com/streamer45/silero-vad-go/speech"
	"google.golang.org/protobuf/proto"
)

// Message type IDs from the ESPHome native API proto (id) option.
// These are not generated as constants by protoc-gen-go and must be defined manually.
const (
	messageTypeHelloRequest                   uint32 = 1
	messageTypeHelloResponse                  uint32 = 2
	messageTypeAuthenticationRequest          uint32 = 3
	messageTypeAuthenticationResponse         uint32 = 4
	messageTypeDisconnectRequest              uint32 = 5
	messageTypeDisconnectResponse             uint32 = 6
	messageTypePingRequest                    uint32 = 7
	messageTypePingResponse                   uint32 = 8
	messageTypeDeviceInfoRequest              uint32 = 9
	messageTypeDeviceInfoResponse             uint32 = 10
	messageTypeListEntitiesRequest            uint32 = 11
	messageTypeListEntitiesDoneResponse       uint32 = 19
	messageTypeSubscribeStatesRequest         uint32 = 20
	messageTypeSubscribeVoiceAssistantRequest uint32 = 89
	messageTypeVoiceAssistantRequest          uint32 = 90
	messageTypeVoiceAssistantResponse         uint32 = 91
	messageTypeVoiceAssistantEventResponse    uint32 = 92
	messageTypeVoiceAssistantAudio            uint32 = 106
)

// pingInterval is how often the server proactively sends PingRequest to the device.
// This keeps the TCP connection alive and lets us detect a silently-dead device faster
// than the readDeadline alone would, because a missed ping response causes the next
// read to time out within readDeadline rather than waiting up to readDeadline from the
// last real message.
const pingInterval = 20 * time.Second

// preSpeechTimeout is how long to wait for speech before giving up. If the VAD has not
// detected any speech within this duration after the pipeline starts, we abort the run
// with "stt-no-text-recognized" rather than making the user wait for the full capture window.
const preSpeechTimeout = 3 * time.Second

// audioCaptureWindow is the hard maximum duration for audio collection. VAD-based
// end-of-speech detection will normally finalize the pipeline sooner, but this cap
// prevents infinite recording in noisy environments where silence never arrives.
const audioCaptureWindow = 15 * time.Second

// vadFrameSize is the number of bytes in one 32ms Silero VAD window at 16kHz 16-bit mono.
// 512 samples * 2 bytes/sample = 1024 bytes.
const vadFrameSize = 1024

// vadSampleRate is the audio sample rate expected by the VAD.
const vadSampleRate = 16000

// speechStartThreshold is the minimum Infer() probability required to begin speech.
// With the correct Silero v5 model, speech probabilities are 0.7-0.99 during speech.
const speechStartThreshold = 0.5

// speechContinueThreshold is the minimum Infer() probability to keep speech active once
// speech has started. Lower than start threshold to avoid mid-sentence cutoffs on brief
// pauses where probability dips.
const speechContinueThreshold = 0.15

// speechStartFramesRequired is the number of consecutive speech frames required before
// speech is considered started. This avoids starting on a single transient spike.
const speechStartFramesRequired = 3

// silenceFramesRequired is the number of consecutive non-speech 32ms frames that must
// follow detected speech before end-of-speech is declared. 1280ms / 32ms = 40 frames.
const silenceFramesRequired = 40

// sttTranscriptTimeout is how long runPipelineResponse waits for the committed_transcript
// message from the ElevenLabs WebSocket before giving up and treating it as an error.
const sttTranscriptTimeout = 10 * time.Second

// settings holds runtime-configurable values that can be loaded from a JSON file and
// overridden by environment variables. The zero value is not valid; use the defaults
// defined in main().
type settings struct {
	SttLanguage string  `json:"stt_language"`
	TtsSpeed    float64 `json:"tts_speed"`
}

// currentSettings is the active settings, protected by settingsMutex.
var currentSettings settings
var settingsMutex sync.RWMutex

// settingsFilePath is the path to the settings JSON file, set from the -settings flag.
var settingsFilePath string

// buildSTTWebSocketURL constructs the ElevenLabs realtime STT WebSocket URL using the
// current SttLanguage setting. Query params select the model, language, audio format,
// and commit strategy. commit_strategy=manual means we control when transcription is
// finalized by sending a chunk with commit=true.
func buildSTTWebSocketURL() string {
	settingsMutex.RLock()
	language := currentSettings.SttLanguage
	settingsMutex.RUnlock()
	return "wss://api.elevenlabs.io/v1/speech-to-text/realtime?model_id=scribe_v2_realtime&language_code=" + url.QueryEscape(language) + "&audio_format=pcm_16000&commit_strategy=manual"
}

// saveSettings writes the given settings to the file at path. The caller is responsible
// for ensuring the settings value is consistent (e.g. by holding settingsMutex before
// reading currentSettings and passing the copy here).
func saveSettings(path string, s settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write settings file: %w", err)
	}
	return nil
}

// readDeadline is the duration after which a read on the TCP connection times out.
// Refreshed after every successful ReadFrame call. When the device silently disappears
// (power loss, no TCP RST), this prevents the server from blocking indefinitely until
// OS-level TCP keepalive fires (~2 hours on Linux).
const readDeadline = 60 * time.Second

// backoffInitial is the starting delay for the reconnect backoff.
const backoffInitial = 1 * time.Second

// backoffMax is the maximum delay between reconnect attempts.
const backoffMax = 60 * time.Second

// detector is the Silero VAD instance used for end-of-speech detection. Initialized once
// at startup by loading the ONNX model from the working directory.
var detector *speech.Detector

// elevenLabsAPIKey is the ElevenLabs API key read from the ELEVENLABS_API_KEY env var at startup.
var elevenLabsAPIKey string

// webhookURL is the full webhook URL (with embedded credentials) read from WEBHOOK_URL at startup.
var webhookURL string

// webhookPayload is the JSON template for the webhook POST body, read from WEBHOOK_PAYLOAD at startup.
// The literal string $transcript is replaced at call time with the JSON-escaped transcript.
var webhookPayload string

// ttsAudioBuffer holds the most recently synthesized TTS mp3 audio, protected by
// ttsAudioMutex so that concurrent HTTP requests and pipeline writes don't race.
var ttsAudioBuffer []byte
var ttsAudioMutex sync.Mutex

// shortcut pairs a compiled regular expression with the URL to POST to when the
// expression matches a transcript. rawPattern holds the original pattern string
// (without the (?i) prefix) so it can be written back to disk without accumulating
// repeated (?i) prefixes across load/save cycles.
type shortcut struct {
	Pattern    *regexp.Regexp
	rawPattern string
	URL        string
}

// shortcuts is the list of active shortcuts, protected by shortcutsMutex.
// A future CRUD API will write to this slice, hence the RWMutex.
var shortcuts []shortcut
var shortcutsMutex sync.RWMutex

// shortcutsFilePath is the path to the shortcuts JSON file, set from the -shortcuts
// flag. It is stored here so the API handlers can pass it to loadShortcuts and
// saveShortcuts without threading it through every call.
var shortcutsFilePath string

// recordDir is the directory path set from the -record-dir flag. When non-empty,
// the server operates in recording mode: utterances are saved as WAV files to this
// directory and the ElevenLabs/webhook env vars are not required.
var recordDir string

// recordingFileCounter is the global sequential file number for recorded utterances.
// It persists across pipeline triggers so that files are numbered continuously across
// multiple wake-word activations (001.wav, 002.wav, ...).
var recordingFileCounter int

// apiPassword is the password required for HTTP Basic Auth on the shortcuts CRUD
// endpoints. Read from API_PASSWORD at startup; required when -shortcuts is set.
var apiPassword string

// compileShortcutPattern strips all leading (?i) prefixes from raw (to clean up
// accumulated prefixes from existing files, e.g. "(?i)(?i)(?i)pattern"), then
// compiles the pattern with a single (?i) prepended so all shortcuts are always
// case-insensitive. It returns the compiled regexp, the cleaned raw pattern
// (without any (?i) prefix), and any error.
func compileShortcutPattern(raw string) (*regexp.Regexp, string, error) {
	cleaned := raw
	for strings.HasPrefix(cleaned, "(?i)") {
		cleaned = strings.TrimPrefix(cleaned, "(?i)")
	}
	compiled, err := regexp.Compile("(?i)" + cleaned)
	if err != nil {
		return nil, "", err
	}
	return compiled, cleaned, nil
}

// loadShortcuts reads the JSON file at path and returns a slice of compiled shortcuts.
// The file format is an array of [regex, url] pairs: [["pattern", "https://..."], ...].
// Returns an error if the file cannot be read, the JSON is malformed, or any regex fails
// to compile. The caller must hold shortcutsMutex (or be the only goroutine accessing
// shortcuts) before calling this function.
func loadShortcuts(path string) ([]shortcut, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shortcuts file: %w", err)
	}

	var raw [][2]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse shortcuts JSON: %w", err)
	}

	result := make([]shortcut, 0, len(raw))
	for index, pair := range raw {
		compiled, cleaned, err := compileShortcutPattern(pair[0])
		if err != nil {
			return nil, fmt.Errorf("shortcuts[%d]: compile regex %q: %w", index, pair[0], err)
		}
		result = append(result, shortcut{Pattern: compiled, rawPattern: cleaned, URL: pair[1]})
	}
	return result, nil
}

// saveShortcuts writes the given shortcuts slice to the file at path.
// The output format matches what loadShortcuts expects: [["pattern", "url"], ...].
// The caller must hold shortcutsMutex before calling this function.
func saveShortcuts(path string, list []shortcut) error {
	raw := make([][2]string, len(list))
	for index, s := range list {
		raw[index] = [2]string{s.rawPattern, s.URL}
	}

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal shortcuts: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write shortcuts file: %w", err)
	}
	return nil
}

// requireAuth checks HTTP Basic Auth against apiPassword. The username is ignored;
// only the password must match. Returns true if auth is valid. On failure it writes
// a 401 response with a WWW-Authenticate header and returns false.
func requireAuth(w http.ResponseWriter, r *http.Request) bool {
	_, password, ok := r.BasicAuth()
	if !ok || password != apiPassword {
		w.Header().Set("WWW-Authenticate", `Basic realm="havpe"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// shortcutsAsJSON serialises the given shortcuts slice as a [][2]string JSON array.
func shortcutsAsJSON(list []shortcut) ([]byte, error) {
	raw := make([][2]string, len(list))
	for index, s := range list {
		raw[index] = [2]string{s.rawPattern, s.URL}
	}

	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal shortcuts: %w", err)
	}
	return data, nil
}

// handleGetShortcuts handles GET /shortcuts. Reads the file from disk, updates the
// in-memory slice, and returns the current list as JSON.
func handleGetShortcuts(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}

	shortcutsMutex.Lock()
	loaded, err := loadShortcuts(shortcutsFilePath)
	if err != nil {
		shortcutsMutex.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	shortcuts = loaded
	list := loaded
	shortcutsMutex.Unlock()

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handlePostShortcuts handles POST /shortcuts. Reads the file from disk, appends a new
// shortcut from the request body (a ["regex", "url"] pair), saves, updates the in-memory
// slice, and returns 201 with the full updated list.
func handlePostShortcuts(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var pair [2]string
	if err := json.Unmarshal(body, &pair); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	compiled, cleaned, err := compileShortcutPattern(pair[0])
	if err != nil {
		http.Error(w, "invalid regex: "+err.Error(), http.StatusBadRequest)
		return
	}

	shortcutsMutex.Lock()
	loaded, err := loadShortcuts(shortcutsFilePath)
	if err != nil {
		shortcutsMutex.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	newIndex := len(loaded)
	loaded = append(loaded, shortcut{Pattern: compiled, rawPattern: cleaned, URL: pair[1]})
	if err := saveShortcuts(shortcutsFilePath, loaded); err != nil {
		shortcutsMutex.Unlock()
		http.Error(w, "save shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	shortcuts = loaded
	list := loaded
	shortcutsMutex.Unlock()

	log.Printf("added shortcut at index %d with regex %q", newIndex, cleaned)

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

// handlePutShortcut handles PUT /shortcuts/{index}. Reads the file from disk, replaces
// the shortcut at the given index with the pair from the request body, saves, updates
// the in-memory slice, and returns 200 with the full updated list. Returns 400 on invalid
// regex, 404 on out-of-range index.
func handlePutShortcut(w http.ResponseWriter, r *http.Request, index int) {
	if !requireAuth(w, r) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var pair [2]string
	if err := json.Unmarshal(body, &pair); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	compiled, cleaned, err := compileShortcutPattern(pair[0])
	if err != nil {
		http.Error(w, "invalid regex: "+err.Error(), http.StatusBadRequest)
		return
	}

	shortcutsMutex.Lock()
	loaded, err := loadShortcuts(shortcutsFilePath)
	if err != nil {
		shortcutsMutex.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if index < 0 || index >= len(loaded) {
		shortcutsMutex.Unlock()
		http.Error(w, "index out of range", http.StatusNotFound)
		return
	}
	loaded[index] = shortcut{Pattern: compiled, rawPattern: cleaned, URL: pair[1]}
	if err := saveShortcuts(shortcutsFilePath, loaded); err != nil {
		shortcutsMutex.Unlock()
		http.Error(w, "save shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	shortcuts = loaded
	list := loaded
	shortcutsMutex.Unlock()

	log.Printf("updated shortcut at index %d with regex %q", index, cleaned)

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleDeleteShortcut handles DELETE /shortcuts/{index}. Reads the file from disk,
// removes the shortcut at the given index, saves, updates the in-memory slice, and
// returns 200 with the full updated list. Returns 404 on out-of-range index.
func handleDeleteShortcut(w http.ResponseWriter, r *http.Request, index int) {
	if !requireAuth(w, r) {
		return
	}

	shortcutsMutex.Lock()
	loaded, err := loadShortcuts(shortcutsFilePath)
	if err != nil {
		shortcutsMutex.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if index < 0 || index >= len(loaded) {
		shortcutsMutex.Unlock()
		http.Error(w, "index out of range", http.StatusNotFound)
		return
	}
	oldPattern := loaded[index].rawPattern
	loaded = append(loaded[:index], loaded[index+1:]...)
	if err := saveShortcuts(shortcutsFilePath, loaded); err != nil {
		shortcutsMutex.Unlock()
		http.Error(w, "save shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	shortcuts = loaded
	list := loaded
	shortcutsMutex.Unlock()

	log.Printf("deleted shortcut at index %d with regex %q", index, oldPattern)

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// handleShortcutsWithIndex dispatches PUT and DELETE requests for /shortcuts/{index}.
// It parses the index from the URL path and routes to the appropriate handler.
func handleShortcutsWithIndex(w http.ResponseWriter, r *http.Request) {
	// Path is /shortcuts/{index}; strip the /shortcuts/ prefix to get the index string.
	indexStr := strings.TrimPrefix(r.URL.Path, "/shortcuts/")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		http.Error(w, "invalid index", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		handlePutShortcut(w, r, index)
	case http.MethodDelete:
		handleDeleteShortcut(w, r, index)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleShortcuts dispatches GET and POST requests for /shortcuts (no index).
func handleShortcuts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleGetShortcuts(w, r)
	case http.MethodPost:
		handlePostShortcuts(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// settingsUpdate is used to decode a PUT /settings request body. Pointer fields allow
// partial updates: a nil pointer means the field was omitted from the request and the
// current value should be kept unchanged.
type settingsUpdate struct {
	SttLanguage *string  `json:"stt_language"`
	TtsSpeed    *float64 `json:"tts_speed"`
}

// handleSettings handles GET and PUT requests for /settings. GET returns the current
// settings as JSON. PUT accepts a partial JSON body and updates only the fields that
// are present, then persists to disk.
func handleSettings(w http.ResponseWriter, r *http.Request) {
	if !requireAuth(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		settingsMutex.RLock()
		data, err := json.Marshal(currentSettings)
		settingsMutex.RUnlock()
		if err != nil {
			http.Error(w, "marshal settings: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var update settingsUpdate
		if err := json.Unmarshal(body, &update); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}

		settingsMutex.Lock()
		candidate := currentSettings
		if update.SttLanguage != nil {
			candidate.SttLanguage = *update.SttLanguage
		}
		if update.TtsSpeed != nil {
			candidate.TtsSpeed = *update.TtsSpeed
		}
		if candidate.SttLanguage == "" {
			settingsMutex.Unlock()
			http.Error(w, "stt_language must not be empty", http.StatusBadRequest)
			return
		}
		if candidate.TtsSpeed <= 0 {
			settingsMutex.Unlock()
			http.Error(w, "tts_speed must be positive", http.StatusBadRequest)
			return
		}
		currentSettings = candidate
		snapshot := currentSettings
		settingsMutex.Unlock()

		if err := saveSettings(settingsFilePath, snapshot); err != nil {
			http.Error(w, "save settings: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("settings updated: stt_language=%s tts_speed=%.2f", snapshot.SttLanguage, snapshot.TtsSpeed)

		data, err := json.Marshal(snapshot)
		if err != nil {
			http.Error(w, "marshal settings: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// tryShortcut checks the transcript against the loaded shortcuts under a read lock.
// The first matching shortcut wins: it POSTs an empty body to the shortcut's URL.
// Returns (true, nil) on a 200 response, (true, err) on a non-200 or network error,
// and (false, nil) when no shortcut matches.
func tryShortcut(transcript string) (bool, error) {
	shortcutsMutex.RLock()
	defer shortcutsMutex.RUnlock()

	for _, shortcut := range shortcuts {
		if !shortcut.Pattern.MatchString(transcript) {
			continue
		}
		log.Printf("shortcut matched %q -> %s", shortcut.Pattern, shortcut.URL)
		response, err := http.Post(shortcut.URL, "", nil)
		if err != nil {
			return true, fmt.Errorf("shortcut POST to %s: %w", shortcut.URL, err)
		}
		// Drain and close the body immediately; we return right after this block.
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return true, fmt.Errorf("shortcut POST to %s returned status %d", shortcut.URL, response.StatusCode)
		}
		return true, nil
	}
	return false, nil
}

func init() {
	cfg := speech.DetectorConfig{
		ModelPath:            "silero_vad.onnx",
		SampleRate:           16000,
		Threshold:            0.5,
		MinSilenceDurationMs: 800,
		SpeechPadMs:          30,
	}
	var err error
	detector, err = speech.NewDetector(cfg)
	if err != nil {
		log.Fatalf("create Silero VAD detector: %v", err)
	}
}

// pipelineState holds all mutable state for a single voice pipeline run.
// It is reset at the start of each new run and zeroed when the run ends.
type pipelineState struct {
	// active is true while we are collecting audio for the current run.
	// It is set to false after runPipelineResponse completes so that straggler
	// audio frames arriving before the device processes RUN_END are silently dropped.
	active bool
	// startTime is when the pipeline run began (i.e. when start=true was received).
	// Audio collection stops once audioCaptureWindow has elapsed since startTime.
	startTime time.Time
	// audioBuffer accumulates PCM chunks from the device.
	audioBuffer []byte
	// vadStartSent tracks whether STT_VAD_START has been sent for this run,
	// so we only send it on the first audio chunk.
	vadStartSent bool
	// vadFrameBuffer holds leftover bytes from the previous audio chunk that did not
	// fill a complete 1024-byte VAD window. Carried forward until the next chunk arrives.
	vadFrameBuffer []byte
	// speechDetected is true once the VAD has classified at least one frame as speech.
	// End-of-speech is only declared after speech has been detected.
	speechDetected bool
	// consecutiveSpeechFrames counts how many consecutive frames crossed the start threshold
	// before speechDetected becomes true.
	consecutiveSpeechFrames int
	// consecutiveSilenceFrames counts how many consecutive non-speech frames have been
	// seen since the last speech frame. Reset to zero whenever a speech frame arrives.
	consecutiveSilenceFrames int
	// lastSpeechEndTime is when the last end-of-speech was detected in recording mode.
	// Zero value means no speech has ended yet in this session, in which case startTime
	// is used for the inter-utterance silence timeout.
	lastSpeechEndTime time.Time
	// sttConn is the WebSocket connection to the ElevenLabs realtime STT API.
	// Opened when the pipeline starts and closed after the transcript is received.
	// Nil when the pipeline is not active or when running in recording mode.
	sttConn *websocket.Conn
	// transcriptChannel receives the committed transcript text from the ElevenLabs
	// WebSocket reader goroutine. Buffered with capacity 1 so the goroutine never blocks.
	transcriptChannel chan string
}

// httpPort is the port on which the HTTP server serves TTS audio files.
// Defined as a const so it is easy to change without hunting through the code.
const httpPort = 8085

// discoverVoicePE browses for _esphomelib._tcp mDNS services for 5 seconds and returns
// the IP address of the first entry whose hostname starts with "home-assistant-voice"
// (case-insensitive). Returns an IP rather than a hostname because .local mDNS names
// don't resolve inside Docker containers. If zero matching devices are found it returns
// an error. If multiple are found it logs all of them and returns an error asking the
// user to set DEVICE_HOST explicitly.
func discoverVoicePE() (string, error) {
	const (
		service        = "_esphomelib._tcp"
		browseTimeout  = 5 * time.Second
		hostnamePrefix = "home-assistant-voice"
	)

	// The entries channel must be buffered and read concurrently: the library sends
	// to it inside Query() without closing it, so we collect in a goroutine and
	// wait for Query to return (which it does after params.Timeout elapses).
	entries := make(chan *mdns.ServiceEntry, 16)
	params := mdns.DefaultParams(service)
	params.Entries = entries
	params.Timeout = browseTimeout

	log.Printf("browsing for %s mDNS services for %v", service, browseTimeout)

	// Keyed by trimmed hostname to de-duplicate responses from the same device
	// arriving on multiple interfaces or as retransmissions.
	matches := make(map[string]net.IP)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for entry := range entries {
			// entry.Host is a FQDN with a trailing dot; trim it for readability and matching.
			host := strings.TrimSuffix(entry.Host, ".")
			if strings.HasPrefix(strings.ToLower(host), hostnamePrefix) {
				addr := entry.AddrV4
				if addr == nil {
					addr = entry.AddrV6
				}
				if addr == nil {
					log.Printf("mDNS found: name=%q host=%q but no IP address, skipping", entry.Name, host)
					continue
				}
				log.Printf("mDNS found: name=%q host=%q addr=%v port=%d", entry.Name, host, addr, entry.Port)
				matches[host] = addr
			}
		}
	}()

	if err := mdns.Query(params); err != nil {
		return "", fmt.Errorf("mDNS query: %w", err)
	}
	// Query has returned (timeout elapsed). Close entries so the collector goroutine exits.
	close(entries)
	<-done

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no %s device found via mDNS; set DEVICE_HOST to specify the host explicitly", hostnamePrefix)
	case 1:
		for host, addr := range matches {
			log.Printf("discovered device: %s (%s)", host, addr)
			// .local mDNS names don't resolve inside Docker, so return the IP address directly.
			return addr.String(), nil
		}
	default:
		log.Printf("multiple matching devices found: %v", matches)
		var hostnames []string
		for host := range matches {
			hostnames = append(hostnames, host)
		}
		return "", fmt.Errorf("multiple %s devices found (%v); set DEVICE_HOST to specify which one to use", hostnamePrefix, hostnames)
	}
	// Unreachable, but required to satisfy the compiler.
	return "", fmt.Errorf("no %s device found via mDNS; set DEVICE_HOST to specify the host explicitly", hostnamePrefix)
}

func main() {
	// Parse CLI flags before anything else so that -shortcuts, -record-dir, and
	// -settings are available during startup.
	flag.StringVar(&shortcutsFilePath, "shortcuts", "", "path to shortcuts JSON file ([regex, url] pairs)")
	flag.StringVar(&recordDir, "record-dir", "", "path to directory for recording utterances as WAV files")
	flag.StringVar(&settingsFilePath, "settings", "settings.json", "path to settings JSON file")
	flag.Parse()

	// Apply settings in precedence order: hardcoded defaults → env vars → settings file.
	// If the settings file doesn't exist, create it with the current values so future
	// runs can edit it directly.
	currentSettings = settings{
		SttLanguage: "en",
		TtsSpeed:    1.0,
	}

	if language := os.Getenv("STT_LANGUAGE"); language != "" {
		currentSettings.SttLanguage = language
	}
	if speedStr := os.Getenv("TTS_SPEED"); speedStr != "" {
		speed, err := strconv.ParseFloat(speedStr, 64)
		if err != nil {
			log.Fatalf("parse TTS_SPEED=%q: %v", speedStr, err)
		}
		currentSettings.TtsSpeed = speed
	}

	if _, err := os.Stat(settingsFilePath); os.IsNotExist(err) {
		if err := saveSettings(settingsFilePath, currentSettings); err != nil {
			log.Fatalf("create settings file %s: %v", settingsFilePath, err)
		}
		log.Printf("created settings file: %s", settingsFilePath)
	} else if err != nil {
		log.Fatalf("stat settings file %s: %v", settingsFilePath, err)
	} else {
		data, err := os.ReadFile(settingsFilePath)
		if err != nil {
			log.Fatalf("read settings file %s: %v", settingsFilePath, err)
		}
		var fileSettings settings
		if err := json.Unmarshal(data, &fileSettings); err != nil {
			log.Fatalf("parse settings file %s: %v", settingsFilePath, err)
		}
		currentSettings = fileSettings
		if currentSettings.SttLanguage == "" {
			currentSettings.SttLanguage = "en"
		}
		if currentSettings.TtsSpeed == 0 {
			currentSettings.TtsSpeed = 1.0
		}
		log.Printf("loaded settings from %s (stt_language=%s, tts_speed=%.2f)", settingsFilePath, currentSettings.SttLanguage, currentSettings.TtsSpeed)
	}

	// Validate required env vars before starting anything. These are permanent failures
	// that cannot be recovered by retrying, so log.Fatalf is appropriate here.
	apiPassword = os.Getenv("API_PASSWORD")
	if apiPassword == "" {
		log.Fatalf("API_PASSWORD environment variable is required")
	}

	// In recording mode the ElevenLabs and webhook vars are unused, so we skip them.
	if recordDir == "" {
		elevenLabsAPIKey = os.Getenv("ELEVENLABS_API_KEY")
		if elevenLabsAPIKey == "" {
			log.Fatalf("ELEVENLABS_API_KEY environment variable is required")
		}

		webhookURL = os.Getenv("WEBHOOK_URL")
		if webhookURL == "" {
			log.Fatalf("WEBHOOK_URL environment variable is required")
		}

		webhookPayload = os.Getenv("WEBHOOK_PAYLOAD")
		if webhookPayload == "" {
			log.Fatalf("WEBHOOK_PAYLOAD environment variable is required")
		}
	} else {
		if err := os.MkdirAll(recordDir, 0o755); err != nil {
			log.Fatalf("create record directory %s: %v", recordDir, err)
		}
		log.Printf("recording mode active: utterances will be saved to %s", recordDir)
	}

	if shortcutsFilePath != "" {
		if _, err := os.Stat(shortcutsFilePath); os.IsNotExist(err) {
			// Create an empty shortcuts file so the user has a valid starting point.
			if err := os.WriteFile(shortcutsFilePath, []byte("[]\n"), 0o644); err != nil {
				log.Fatalf("create shortcuts file %s: %v", shortcutsFilePath, err)
			}
			log.Printf("created empty shortcuts file: %s", shortcutsFilePath)
		} else {
			loaded, err := loadShortcuts(shortcutsFilePath)
			if err != nil {
				log.Fatalf("load shortcuts from %s: %v", shortcutsFilePath, err)
			}
			shortcutsMutex.Lock()
			shortcuts = loaded
			shortcutsMutex.Unlock()
			log.Printf("loaded %d shortcut(s) from %s", len(shortcuts), shortcutsFilePath)
		}

		// Register CRUD endpoints only when shortcuts are enabled. The /shortcuts/
		// prefix handler covers PUT and DELETE (which include an index), while the
		// exact /shortcuts match covers GET and POST.
		http.HandleFunc("/shortcuts/", handleShortcutsWithIndex)
		http.HandleFunc("/shortcuts", handleShortcuts)
		log.Printf("shortcuts CRUD API enabled at /shortcuts")
	}

	// Register the settings API unconditionally so it is always available regardless of
	// whether shortcuts or recording mode are enabled.
	http.HandleFunc("/settings", handleSettings)
	log.Printf("settings API enabled at /settings")

	defer func() {
		if err := detector.Destroy(); err != nil {
			log.Printf("destroy VAD detector: %v", err)
		}
	}()

	// Serve tone.wav and error.wav from the working directory so the device can fetch them
	// via its media player. The Content-Type header is set explicitly because http.ServeFile
	// may sniff the wrong type for some WAV files.
	http.HandleFunc("/tone.wav", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		http.ServeFile(w, r, "tone.wav")
	})
	http.HandleFunc("/error.wav", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		http.ServeFile(w, r, "error.wav")
	})
	// /tts.mp3 serves the most recently synthesized TTS audio. The buffer is written
	// before the device is told to fetch this URL, so a race between write and read
	// is not possible in normal operation; the mutex guards against any edge cases.
	http.HandleFunc("/tts.mp3", func(w http.ResponseWriter, r *http.Request) {
		ttsAudioMutex.Lock()
		audio := make([]byte, len(ttsAudioBuffer))
		copy(audio, ttsAudioBuffer)
		ttsAudioMutex.Unlock()
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write(audio)
	})
	go func() {
		listenAddress := fmt.Sprintf("0.0.0.0:%d", httpPort)
		log.Printf("starting HTTP server on %s", listenAddress)
		if err := http.ListenAndServe(listenAddress, nil); err != nil {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	// staticHost is non-empty when DEVICE_HOST is set. In that case we skip mDNS
	// discovery on every iteration and use the fixed address instead.
	staticHost := os.Getenv("DEVICE_HOST")
	if staticHost != "" {
		log.Printf("using DEVICE_HOST=%s", staticHost)
	}

	backoff := backoffInitial
	attempt := 0

	for {
		attempt++

		host := staticHost
		if host == "" {
			var err error
			host, err = discoverVoicePE()
			if err != nil {
				log.Printf("device discovery (attempt %d): %v", attempt, err)
				log.Printf("retrying in %v", backoff)
				time.Sleep(backoff)
				backoff = min(backoff*2, backoffMax)
				continue
			}
		}

		address := host + ":6053"
		log.Printf("connecting to %s (attempt %d)", address, attempt)
		conn, err := net.Dial("tcp", address)
		if err != nil {
			log.Printf("dial %s (attempt %d): %v", address, attempt, err)
			log.Printf("retrying in %v", backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, backoffMax)
			continue
		}
		log.Printf("connected to %s", address)

		// ttsURL and friends depend on the local address, which may differ across
		// reconnections (e.g. if the network interface changes).
		localIP := conn.LocalAddr().(*net.TCPAddr).IP.String()
		ttsURL := fmt.Sprintf("http://%s:%d/tone.wav", localIP, httpPort)
		errorURL := fmt.Sprintf("http://%s:%d/error.wav", localIP, httpPort)
		ttsResponseURL := fmt.Sprintf("http://%s:%d/tts.mp3", localIP, httpPort)
		log.Printf("TTS tone URL: %s", ttsURL)
		log.Printf("TTS error URL: %s", errorURL)
		log.Printf("TTS response URL: %s", ttsResponseURL)

		if err := handleConnection(conn, ttsURL, errorURL, ttsResponseURL, func() {
			// Called by handleConnection when the handshake completes and the read loop
			// starts. At this point the connection is healthy, so reset the backoff so
			// that the next failure (if any) starts from the minimum delay again.
			log.Printf("connection healthy, resetting backoff to %v", backoffInitial)
			backoff = backoffInitial
		}); err != nil {
			log.Printf("connection ended with error (attempt %d): %v", attempt, err)
			log.Printf("retrying in %v", backoff)
			time.Sleep(backoff)
			backoff = min(backoff*2, backoffMax)
		} else {
			// Clean disconnect (DisconnectRequest from device): reconnect immediately.
			// Backoff was already reset when the read loop started.
			log.Printf("connection closed cleanly, reconnecting immediately")
		}
	}
}

// handleConnection performs the ESPHome handshake and then runs the main read loop until
// the connection ends. It returns nil on a clean disconnect (DisconnectRequest) and a
// non-nil error on any failure (dial error, handshake failure, read timeout, etc.).
// onConnected is called once the handshake completes and the read loop starts; the caller
// uses this to reset the reconnect backoff. The caller is responsible for retrying.
func handleConnection(conn net.Conn, ttsURL string, errorURL string, ttsResponseURL string, onConnected func()) error {
	defer conn.Close()

	// done is closed when handleConnection returns, signalling the ping goroutine to exit.
	done := make(chan struct{})
	defer close(done)

	// lw serialises all writes to conn. The read loop and the ping goroutine both write
	// to the same TCP connection, so without this mutex their frame bytes would interleave.
	lw := &lockedWriter{conn: conn}

	// Set the initial read deadline before the handshake. It is refreshed after every
	// successful ReadFrame call so that a healthy but slow device does not time out.
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
		return fmt.Errorf("set initial read deadline: %w", err)
	}

	// bufio.Reader is required so that ReadFrame can use it as an io.ByteReader
	// for varint decoding without wrapping it again internally.
	reader := bufio.NewReader(conn)

	// Perform the handshake before entering the read loop. The device may send
	// PingRequest at any point during the handshake; readHandshakeResponse handles
	// those transparently.

	// Step 1: client sends HelloRequest to identify itself.
	if err := sendMessage(lw, messageTypeHelloRequest, &api.HelloRequest{
		ClientInfo:      "esphome-go-server",
		ApiVersionMajor: 1,
		ApiVersionMinor: 10,
	}); err != nil {
		return fmt.Errorf("send HelloRequest: %w", err)
	}

	// Step 2: device replies with HelloResponse.
	helloData, err := readHandshakeResponse(conn, lw, reader, messageTypeHelloResponse)
	if err != nil {
		return fmt.Errorf("hello handshake: %w", err)
	}
	var helloResponse api.HelloResponse
	if err := proto.Unmarshal(helloData, &helloResponse); err != nil {
		return fmt.Errorf("unmarshal HelloResponse: %w", err)
	}
	log.Printf("HelloResponse: server_info=%q name=%q api_version=%d.%d",
		helloResponse.GetServerInfo(), helloResponse.GetName(),
		helloResponse.GetApiVersionMajor(), helloResponse.GetApiVersionMinor())

	// Step 3: client requests device info.
	// Authentication (message IDs 3-4) was removed in ESPHome 2026.1.0; sending
	// AuthenticationRequest causes the handshake to hang because the device never
	// replies with AuthenticationResponse.
	if err := sendMessage(lw, messageTypeDeviceInfoRequest, &api.DeviceInfoRequest{}); err != nil {
		return fmt.Errorf("send DeviceInfoRequest: %w", err)
	}

	// Step 4: device replies with DeviceInfoResponse.
	deviceInfoData, err := readHandshakeResponse(conn, lw, reader, messageTypeDeviceInfoResponse)
	if err != nil {
		return fmt.Errorf("device info handshake: %w", err)
	}
	var deviceInfoResponse api.DeviceInfoResponse
	if err := proto.Unmarshal(deviceInfoData, &deviceInfoResponse); err != nil {
		return fmt.Errorf("unmarshal DeviceInfoResponse: %w", err)
	}
	log.Printf("DeviceInfoResponse: name=%q model=%q", deviceInfoResponse.GetName(), deviceInfoResponse.GetModel())

	// Step 5: client requests entity list; device sends multiple ListEntities* messages
	// followed by ListEntitiesDoneResponse (ID 19).
	if err := sendMessage(lw, messageTypeListEntitiesRequest, &api.ListEntitiesRequest{}); err != nil {
		return fmt.Errorf("send ListEntitiesRequest: %w", err)
	}
	if err := drainEntityList(conn, lw, reader); err != nil {
		return fmt.Errorf("entity list: %w", err)
	}

	// Step 6: subscribe to state updates and voice assistant events.
	if err := sendMessage(lw, messageTypeSubscribeStatesRequest, &api.SubscribeStatesRequest{}); err != nil {
		return fmt.Errorf("send SubscribeStatesRequest: %w", err)
	}
	// flags=1 requests API audio streaming (audio sent over the API connection, not UDP).
	if err := sendMessage(lw, messageTypeSubscribeVoiceAssistantRequest, &api.SubscribeVoiceAssistantRequest{
		Subscribe: true,
		Flags:     1,
	}); err != nil {
		return fmt.Errorf("send SubscribeVoiceAssistantRequest: %w", err)
	}

	// Handshake complete: notify the caller that the connection is healthy so it can
	// reset the reconnect backoff. This must happen before the read loop so that even
	// a connection that drops immediately after the handshake resets the backoff.
	log.Printf("handshake complete, entering read loop")
	onConnected()

	// Start the ping goroutine after the handshake so that the device knows we are
	// alive even during long silences (e.g. no voice activity). The goroutine exits
	// when done is closed (i.e. when handleConnection returns). Write errors from the
	// ping goroutine are not propagated back here; the read loop will detect the dead
	// connection via its read deadline and return an error naturally.
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := sendMessage(lw, messageTypePingRequest, &api.PingRequest{}); err != nil {
					log.Printf("ping goroutine: send PingRequest: %v", err)
					// Close the connection so the read loop unblocks and returns
					// promptly rather than waiting up to readDeadline in a state
					// where pings can no longer be sent.
					conn.Close()
					return
				}
			}
		}
	}()

	var pipeline pipelineState

	for {
		messageType, data, err := readFrameWithDeadline(conn, reader)
		if err != nil {
			if err == io.EOF {
				log.Println("connection closed by device")
			} else {
				log.Printf("read frame: %v", err)
			}
			return err
		}

		switch messageType {
		case messageTypePingRequest:
			if err := handlePingRequest(lw); err != nil {
				return err
			}
		case messageTypePingResponse:
			// No-op: this is the device's reply to our proactive PingRequest.
		case messageTypeDisconnectRequest:
			// Best-effort: send DisconnectResponse, but ignore write errors since we're
			// closing the connection regardless.
			handleDisconnectRequest(lw)
			return nil
		case messageTypeVoiceAssistantRequest:
			if err := handleVoiceAssistantRequest(lw, data, &pipeline); err != nil {
				return err
			}
		case messageTypeVoiceAssistantAudio:
			if err := handleVoiceAssistantAudio(lw, data, &pipeline, ttsURL, errorURL, ttsResponseURL); err != nil {
				return err
			}
		default:
			log.Printf("ignoring message type %d", messageType)
		}
	}
}

// readFrameWithDeadline reads one frame and refreshes the read deadline on success.
// Keeping the deadline refresh next to the ReadFrame call makes it obvious that every
// successful read extends the window, and that a silent device will eventually time out.
func readFrameWithDeadline(conn net.Conn, reader *bufio.Reader) (uint32, []byte, error) {
	messageType, data, err := ReadFrame(reader)
	if err != nil {
		return 0, nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(readDeadline)); err != nil {
		return 0, nil, fmt.Errorf("refresh read deadline: %w", err)
	}
	return messageType, data, nil
}

// readHandshakeResponse reads frames from reader until a frame with the expected message
// type is received. It returns the body bytes of that frame.
//
// PingRequest frames are answered transparently. Any other unexpected frame type is logged
// and skipped rather than treated as an error, because the device may send state updates or
// service requests at any point during the handshake.
func readHandshakeResponse(conn net.Conn, writer io.Writer, reader *bufio.Reader, expectedType uint32) ([]byte, error) {
	for {
		messageType, data, err := readFrameWithDeadline(conn, reader)
		if err != nil {
			return nil, fmt.Errorf("reading frame (expected type %d): %w", expectedType, err)
		}
		if messageType == messageTypePingRequest {
			if err := handlePingRequest(writer); err != nil {
				return nil, err
			}
			continue
		}
		if messageType != expectedType {
			log.Printf("readHandshakeResponse: skipping unexpected message type %d while waiting for type %d", messageType, expectedType)
			continue
		}
		return data, nil
	}
}

// drainEntityList reads frames until ListEntitiesDoneResponse (ID 19) is received,
// responding to PingRequest frames along the way. Entity list frames are logged and ignored.
func drainEntityList(conn net.Conn, writer io.Writer, reader *bufio.Reader) error {
	for {
		messageType, _, err := readFrameWithDeadline(conn, reader)
		if err != nil {
			return fmt.Errorf("reading entity list frame: %w", err)
		}
		if messageType == messageTypePingRequest {
			if err := handlePingRequest(writer); err != nil {
				return err
			}
			continue
		}
		if messageType == messageTypeListEntitiesDoneResponse {
			log.Printf("ListEntitiesDoneResponse received")
			return nil
		}
		log.Printf("ignoring entity list message type %d", messageType)
	}
}

// lockedWriter wraps a net.Conn with a mutex so that concurrent goroutines (the read
// loop and the ping goroutine) never interleave partial frame writes on the same TCP
// connection. All writes go through this type; reads use the underlying net.Conn directly.
type lockedWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.conn.Write(p)
}

func sendMessage(writer io.Writer, messageType uint32, message proto.Message) error {
	data, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal message type %d: %w", messageType, err)
	}
	if err := WriteFrame(writer, messageType, data); err != nil {
		return fmt.Errorf("write frame type %d: %w", messageType, err)
	}
	return nil
}

func handlePingRequest(writer io.Writer) error {
	return sendMessage(writer, messageTypePingResponse, &api.PingResponse{})
}

func handleDisconnectRequest(writer io.Writer) {
	log.Printf("DisconnectRequest received, sending DisconnectResponse and closing")
	// Ignore the error: we are closing the connection regardless of whether the
	// response was delivered.
	_ = sendMessage(writer, messageTypeDisconnectResponse, &api.DisconnectResponse{})
}

// handleVoiceAssistantRequest handles message type 90. When start=true it begins a new
// pipeline run: resets the pipeline state, records the start time, sends
// VoiceAssistantResponse with port=0 (API audio mode), and sends the RUN_START and
// STT_START events. When start=false the device has cancelled the run; we just reset state.
func handleVoiceAssistantRequest(writer io.Writer, data []byte, pipeline *pipelineState) error {
	var request api.VoiceAssistantRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		log.Printf("unmarshal VoiceAssistantRequest: %v", err)
		*pipeline = pipelineState{}
		return nil
	}

	if !request.GetStart() {
		log.Printf("VoiceAssistantRequest start=false: pipeline cancelled by device, resetting state")
		// Close the WebSocket before zeroing the struct so the readSTTMessages goroutine
		// unblocks and exits rather than leaking.
		closeSTTConnection(pipeline)
		*pipeline = pipelineState{}
		// The device may be stuck in AWAITING_RESPONSE or another non-idle state after
		// sending start=false. Sending RUN_END ensures its state machine reaches IDLE and
		// the on_end trigger fires to reset LEDs.
		return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	log.Printf("VoiceAssistantRequest start=true: beginning pipeline run (wake_word=%q flags=%d)",
		request.GetWakeWordPhrase(), request.GetFlags())

	*pipeline = pipelineState{
		active:            true,
		startTime:         time.Now(),
		transcriptChannel: make(chan string, 1),
	}

	// Reset the VAD detector state so that state from a previous pipeline run does not
	// bleed into the new one.
	if err := detector.Reset(); err != nil {
		log.Printf("reset VAD detector: %v", err)
	}

	// Open the ElevenLabs realtime STT WebSocket. We do this eagerly so that audio
	// chunks can be streamed as they arrive rather than batched after end-of-speech.
	// Recording mode does not use the WebSocket (it saves WAV files instead), so we
	// skip this when recordDir is set. We also skip when elevenLabsAPIKey is empty,
	// which happens in tests where main() is never called.
	if recordDir == "" && elevenLabsAPIKey != "" {
		sttURL, err := url.Parse(buildSTTWebSocketURL())
		if err != nil {
			// A parse error here means the language setting produced an invalid URL, which
			// should not happen since we use url.QueryEscape on the language value.
			log.Fatalf("parse ElevenLabs STT WebSocket URL: %v", err)
		}
		dialer := websocket.Dialer{}
		headers := http.Header{}
		headers.Set("xi-api-key", elevenLabsAPIKey)
		sttConn, _, err := dialer.Dial(sttURL.String(), headers)
		if err != nil {
			log.Printf("open ElevenLabs STT WebSocket: %v", err)
			// Close the channel immediately so waitForTranscript fails fast rather than
			// blocking for the full sttTranscriptTimeout.
			close(pipeline.transcriptChannel)
		} else {
			pipeline.sttConn = sttConn
			// Start the reader goroutine. It reads messages until the connection closes and
			// sends the first committed_transcript text to transcriptChannel.
			go readSTTMessages(sttConn, pipeline.transcriptChannel)
		}
	}

	// port=0 tells the device to stream audio over the API connection rather than UDP.
	if err := sendMessage(writer, messageTypeVoiceAssistantResponse, &api.VoiceAssistantResponse{Port: 0}); err != nil {
		return err
	}

	if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_START, nil); err != nil {
		return err
	}
	return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_START, nil)
}

// closeSTTConnection closes pipeline.sttConn if it is non-nil and nils the field.
// Closing the connection causes the readSTTMessages goroutine's ReadMessage call to
// return an error, which unblocks it so it can exit cleanly.
func closeSTTConnection(pipeline *pipelineState) {
	if pipeline.sttConn != nil {
		pipeline.sttConn.Close()
		pipeline.sttConn = nil
	}
}

// readSTTMessages reads messages from the ElevenLabs realtime STT WebSocket until the
// connection closes. When a committed_transcript message arrives, its text is sent to
// transcriptChannel and the function returns. If the connection drops before a transcript
// arrives, the channel is closed so waitForTranscript can fail fast instead of waiting
// for the full timeout.
func readSTTMessages(conn *websocket.Conn, transcriptChannel chan string) {
	// Short voice commands sometimes produce partial_transcript messages but an empty
	// committed_transcript. We track the last partial so we can fall back to it.
	lastPartialText := ""
	for {
		_, messageBytes, err := conn.ReadMessage()
		if err != nil {
			// The connection dropped before a committed_transcript arrived (or was closed
			// by closeSTTConnection on a non-finalization path like pre-speech timeout).
			// Closing the channel lets waitForTranscript fail immediately.
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
				// Channel already has a value (shouldn't happen in normal flow, but guard anyway).
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

// handleVoiceAssistantAudio handles message type 106. It accumulates PCM chunks into
// pipeline.audioBuffer while the pipeline is active. Each incoming chunk is fed through
// the Silero VAD in 1024-byte (32ms, 512-sample) windows to detect end-of-speech. The
// pipeline is finalized when 800ms of consecutive silence follows detected speech, or
// when the hard audioCaptureWindow maximum is reached. Audio frames arriving when the
// pipeline is inactive are silently ignored.
func handleVoiceAssistantAudio(writer io.Writer, data []byte, pipeline *pipelineState, ttsURL string, errorURL string, ttsResponseURL string) error {
	if !pipeline.active {
		log.Printf("audio chunk received but no active pipeline, ignoring")
		return nil
	}

	if recordDir != "" {
		return handleRecordingAudio(writer, data, pipeline)
	}

	var chunk api.VoiceAssistantAudio
	if err := proto.Unmarshal(data, &chunk); err != nil {
		log.Printf("unmarshal VoiceAssistantAudio: %v", err)
		return nil
	}

	if !pipeline.vadStartSent {
		log.Printf("first audio chunk received, sending STT_VAD_START")
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_START, nil); err != nil {
			return err
		}
		pipeline.vadStartSent = true
	}

	chunkData := chunk.GetData()
	pipeline.audioBuffer = append(pipeline.audioBuffer, chunkData...)

	// Stream the chunk to the ElevenLabs realtime STT WebSocket so transcription can
	// proceed concurrently with VAD processing. On failure, nil out sttConn so subsequent
	// chunks don't keep trying and spamming the log.
	if pipeline.sttConn != nil {
		if !sendSTTChunk(pipeline.sttConn, chunkData, false) {
			pipeline.sttConn = nil
		}
	}

	// Prepend any leftover bytes from the previous chunk, then process complete VAD windows.
	// Each window is vadFrameSize bytes (512 samples at 16kHz, 16-bit LE = 32ms of audio).
	// Infer() requires exactly 512 samples per call and maintains RNN state across calls,
	// so we must feed it one fixed-size window at a time rather than variable-length buffers.
	pipeline.vadFrameBuffer = append(pipeline.vadFrameBuffer, chunkData...)
	for len(pipeline.vadFrameBuffer) >= vadFrameSize {
		frame := pipeline.vadFrameBuffer[:vadFrameSize]
		pipeline.vadFrameBuffer = pipeline.vadFrameBuffer[vadFrameSize:]

		// Convert 16-bit signed LE PCM bytes to float32 samples normalised to [-1, 1].
		samples := make([]float32, vadFrameSize/2)
		for i := range samples {
			sample := int16(binary.LittleEndian.Uint16(frame[i*2 : i*2+2]))
			samples[i] = float32(sample) / 32768.0
		}

		probability, err := detector.Infer(samples)
		if err != nil {
			log.Printf("VAD infer error: %v", err)
			continue
		}

		if !pipeline.speechDetected {
			if probability >= speechStartThreshold {
				pipeline.consecutiveSpeechFrames++
				if pipeline.consecutiveSpeechFrames >= speechStartFramesRequired {
					pipeline.speechDetected = true
					pipeline.consecutiveSilenceFrames = 0
					log.Printf("speech start detected after %d consecutive frames", pipeline.consecutiveSpeechFrames)
				}
			} else {
				pipeline.consecutiveSpeechFrames = 0
			}
			continue
		}

		if probability >= speechContinueThreshold {
			pipeline.consecutiveSilenceFrames = 0
		} else {
			pipeline.consecutiveSilenceFrames++
			if pipeline.consecutiveSilenceFrames >= silenceFramesRequired {
				log.Printf("VAD end-of-speech detected after %d silence frames, finalising pipeline",
					pipeline.consecutiveSilenceFrames)
				pipeline.active = false
				// Commit the transcript: send a final chunk with commit=true so ElevenLabs
				// finalizes transcription. An empty audio payload is fine for the commit message.
				if pipeline.sttConn != nil {
					if !sendSTTChunk(pipeline.sttConn, nil, true) {
						pipeline.sttConn = nil
					}
				}
				return runPipelineResponse(writer, pipeline, ttsURL, errorURL, ttsResponseURL)
			}
		}
	}

	elapsed := time.Since(pipeline.startTime)

	if !pipeline.speechDetected && elapsed >= preSpeechTimeout {
		log.Printf("no speech detected after %v, aborting pipeline", preSpeechTimeout)
		pipeline.active = false
		// No transcript will ever arrive, so close the WebSocket now to unblock the
		// readSTTMessages goroutine rather than leaving it running until the connection
		// is eventually dropped by the server.
		closeSTTConnection(pipeline)
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_END, nil); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END, []*api.VoiceAssistantEventData{
			{Name: "text", Value: ""},
		}); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "stt-no-text-recognized"},
			{Name: "message", Value: "No speech detected"},
		}); err != nil {
			return err
		}
		return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	if elapsed >= audioCaptureWindow {
		// Hard safety cap: finalize even if VAD never triggered end-of-speech.
		log.Printf("audio capture window elapsed, finalising pipeline")
		pipeline.active = false
		// Commit the transcript even on the hard cap so ElevenLabs finalizes what it has.
		if pipeline.sttConn != nil {
			if !sendSTTChunk(pipeline.sttConn, nil, true) {
				pipeline.sttConn = nil
			}
		}
		return runPipelineResponse(writer, pipeline, ttsURL, errorURL, ttsResponseURL)
	}

	return nil
}

// interUtteranceSilenceTimeout is how long to wait between utterances in recording mode
// before ending the session. This replaces the normal preSpeechTimeout for the
// inter-utterance gap, and is longer to give the speaker time to pause between words.
const interUtteranceSilenceTimeout = 5 * time.Second

// handleRecordingAudio handles audio chunks in recording mode. It buffers all audio for
// the entire session and writes one WAV file when the session ends. VAD is used only to
// track when speech last ended, for the inter-utterance silence timeout. The session ends
// after 5 seconds of silence since the last end-of-speech (or since session start if no
// speech has been detected yet).
func handleRecordingAudio(writer io.Writer, data []byte, pipeline *pipelineState) error {
	var chunk api.VoiceAssistantAudio
	if err := proto.Unmarshal(data, &chunk); err != nil {
		log.Printf("unmarshal VoiceAssistantAudio: %v", err)
		return nil
	}

	if !pipeline.vadStartSent {
		log.Printf("first audio chunk received in recording mode, sending STT_VAD_START")
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_START, nil); err != nil {
			return err
		}
		pipeline.vadStartSent = true
	}

	chunkData := chunk.GetData()
	pipeline.audioBuffer = append(pipeline.audioBuffer, chunkData...)
	// Same VAD frame processing as the normal pipeline: feed 1024-byte windows to the
	// detector and track consecutive speech/silence frames.
	pipeline.vadFrameBuffer = append(pipeline.vadFrameBuffer, chunkData...)
	for len(pipeline.vadFrameBuffer) >= vadFrameSize {
		frame := pipeline.vadFrameBuffer[:vadFrameSize]
		pipeline.vadFrameBuffer = pipeline.vadFrameBuffer[vadFrameSize:]

		samples := make([]float32, vadFrameSize/2)
		for i := range samples {
			sample := int16(binary.LittleEndian.Uint16(frame[i*2 : i*2+2]))
			samples[i] = float32(sample) / 32768.0
		}

		probability, err := detector.Infer(samples)
		if err != nil {
			log.Printf("VAD infer error: %v", err)
			continue
		}

		if !pipeline.speechDetected {
			if probability >= speechStartThreshold {
				pipeline.consecutiveSpeechFrames++
				if pipeline.consecutiveSpeechFrames >= speechStartFramesRequired {
					pipeline.speechDetected = true
					pipeline.consecutiveSilenceFrames = 0
					log.Printf("speech start detected after %d consecutive frames", pipeline.consecutiveSpeechFrames)
				}
			} else {
				pipeline.consecutiveSpeechFrames = 0
			}
			continue
		}

		if probability >= speechContinueThreshold {
			pipeline.consecutiveSilenceFrames = 0
		} else {
			pipeline.consecutiveSilenceFrames++
			if pipeline.consecutiveSilenceFrames >= silenceFramesRequired {
				log.Printf("VAD end-of-speech detected after %d silence frames", pipeline.consecutiveSilenceFrames)
				// Record when speech ended so the silence timeout is measured from here.
				// audioBuffer and vadFrameBuffer are intentionally kept intact — the whole
				// session is saved as one WAV at the end, not per utterance.
				pipeline.lastSpeechEndTime = time.Now()
				pipeline.speechDetected = false
				pipeline.consecutiveSpeechFrames = 0
				pipeline.consecutiveSilenceFrames = 0
			}
		}
	}

	// Check the inter-utterance silence timeout. We only apply this when no speech has
	// been detected since the last end-of-speech (i.e. we are in a silence gap).
	// Once speech starts again, we wait for the next end-of-speech via the VAD loop above.
	if !pipeline.speechDetected {
		// Use lastSpeechEndTime if speech has ended at least once; otherwise use
		// startTime so a session with no speech at all also times out.
		referenceTime := pipeline.lastSpeechEndTime
		if referenceTime.IsZero() {
			referenceTime = pipeline.startTime
		}
		if time.Since(referenceTime) >= interUtteranceSilenceTimeout {
			log.Printf("inter-utterance silence timeout after %v, ending session", interUtteranceSilenceTimeout)
			// Only write a file if we actually captured speech. A session that times
			// out with pure silence (e.g. accidental trigger) produces no WAV.
			if !pipeline.lastSpeechEndTime.IsZero() {
				filename := fmt.Sprintf("%03d.wav", recordingFileCounter+1)
				filePath := filepath.Join(recordDir, filename)
				if err := os.WriteFile(filePath, buildWAV(pipeline.audioBuffer), 0o644); err != nil {
					log.Printf("write WAV file %s: %v", filePath, err)
				} else {
					recordingFileCounter++
					log.Printf("saved session recording to %s", filePath)
				}
			}
			pipeline.active = false
			if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_END, nil); err != nil {
				return err
			}
			if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END, []*api.VoiceAssistantEventData{
				{Name: "text", Value: ""},
			}); err != nil {
				return err
			}
			return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
		}
	}

	return nil
}

// buildWAV wraps raw 16-bit signed LE PCM data (16kHz, mono) in a standard RIFF/WAVE
// header and returns the complete WAV file as a byte slice. The header constants match
// the audio format produced by the ESPHome voice assistant pipeline.
func buildWAV(pcmData []byte) []byte {
	const (
		sampleRate    = 16000
		channels      = 1
		bitsPerSample = 16
		audioFormat   = 1 // PCM
	)
	dataSize := uint32(len(pcmData))
	byteRate := uint32(sampleRate * channels * bitsPerSample / 8)
	blockAlign := uint16(channels * bitsPerSample / 8)

	// RIFF/WAVE header: 44 bytes total. The four-character chunk IDs ("RIFF", "WAVE",
	// "fmt ", "data") are written as raw bytes rather than via binary.Write so that
	// binary.BigEndian is not needed just for ASCII literals.
	header := struct {
		ChunkID       [4]byte
		ChunkSize     uint32
		Format        [4]byte
		Subchunk1ID   [4]byte
		Subchunk1Size uint32
		AudioFormat   uint16
		NumChannels   uint16
		SampleRate    uint32
		ByteRate      uint32
		BlockAlign    uint16
		BitsPerSample uint16
		Subchunk2ID   [4]byte
		Subchunk2Size uint32
	}{
		ChunkID:       [4]byte{'R', 'I', 'F', 'F'},
		ChunkSize:     36 + dataSize, // 36 = header size minus the 8 bytes for ChunkID and ChunkSize
		Format:        [4]byte{'W', 'A', 'V', 'E'},
		Subchunk1ID:   [4]byte{'f', 'm', 't', ' '},
		Subchunk1Size: 16,
		AudioFormat:   audioFormat,
		NumChannels:   channels,
		SampleRate:    sampleRate,
		ByteRate:      byteRate,
		BlockAlign:    blockAlign,
		BitsPerSample: bitsPerSample,
		Subchunk2ID:   [4]byte{'d', 'a', 't', 'a'},
		Subchunk2Size: dataSize,
	}

	var wavBuffer bytes.Buffer
	// binary.Write errors only when the destination writer fails or the value is not
	// a fixed-size type. Both conditions are impossible here, so we ignore the error.
	_ = binary.Write(&wavBuffer, binary.LittleEndian, header)
	_, _ = wavBuffer.Write(pcmData)
	return wavBuffer.Bytes()
}

// sendSTTChunk sends a single PCM audio chunk to the ElevenLabs realtime STT WebSocket.
// When commit is true, the message signals ElevenLabs to finalize transcription. Returns
// true on success, false on any error (which is also logged). The caller should nil out
// pipeline.sttConn on false to avoid log spam from subsequent failed sends.
func sendSTTChunk(conn *websocket.Conn, audioData []byte, commit bool) bool {
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

// waitForTranscript waits up to sttTranscriptTimeout for the committed transcript from the
// ElevenLabs realtime STT WebSocket. Returns the transcript text, or an error if the
// timeout elapses, the channel is closed (connection dropped), or the pipeline has no
// transcript channel.
func waitForTranscript(pipeline *pipelineState) (string, error) {
	if pipeline.transcriptChannel == nil {
		return "", fmt.Errorf("no STT transcript channel (pipeline not initialized)")
	}
	select {
	case transcript, ok := <-pipeline.transcriptChannel:
		if !ok {
			// Channel was closed by readSTTMessages because the connection dropped before
			// a committed_transcript arrived.
			return "", fmt.Errorf("STT WebSocket connection closed before transcript arrived")
		}
		return transcript, nil
	case <-time.After(sttTranscriptTimeout):
		return "", fmt.Errorf("timed out waiting for STT transcript after %v", sttTranscriptTimeout)
	}
}

// postWebhook sends the transcript to the configured webhook URL as a JSON POST request.
// It returns the "response" field from the JSON response body, or "" if the field is absent.
func postWebhook(transcript string) (string, error) {
	// json.Marshal on a string produces a quoted JSON string (e.g. `"hello \"world\""`).
	// Stripping the surrounding quotes gives us the escaped content safe to embed in JSON.
	escapedBytes, err := json.Marshal(transcript)
	if err != nil {
		return "", fmt.Errorf("JSON-escape transcript: %w", err)
	}
	escaped := string(escapedBytes[1 : len(escapedBytes)-1])
	payloadBody := strings.Replace(webhookPayload, "$transcript", escaped, 1)

	response, err := http.Post(webhookURL, "application/json", bytes.NewReader([]byte(payloadBody)))
	if err != nil {
		return "", fmt.Errorf("webhook POST: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return "", fmt.Errorf("webhook returned status %d: %s", response.StatusCode, body)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read webhook response body: %w", err)
	}
	log.Printf("webhook response: %s", body)

	var result struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// A non-JSON body (e.g. plain "OK") is not an error; the response text is just empty.
		log.Printf("webhook response is not JSON, treating as empty response: %v", err)
		return "", nil
	}

	return result.Response, nil
}

// runPipelineResponse waits for the committed transcript from the ElevenLabs realtime STT
// WebSocket, posts it to the webhook, and sends the remaining pipeline events to the device.
// When the webhook returns a non-empty response text, it is synthesized to speech and the
// device plays ttsResponseURL; otherwise the device plays ttsURL (tone.wav). On any failure
// it plays errorURL with a VOICE_ASSISTANT_ERROR event. On the first write error, it stops
// sending further events and returns immediately.
func runPipelineResponse(writer io.Writer, pipeline *pipelineState, ttsURL string, errorURL string, ttsResponseURL string) error {
	if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_END, nil); err != nil {
		return err
	}

	transcript, transcriptErr := waitForTranscript(pipeline)
	closeSTTConnection(pipeline)

	if transcriptErr != nil {
		log.Printf("STT error: %v", transcriptErr)
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "pipeline-error"},
			{Name: "message", Value: transcriptErr.Error()},
		}); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
			{Name: "url", Value: errorURL},
		}); err != nil {
			return err
		}
		return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	if strings.TrimSpace(transcript) == "" {
		log.Printf("STT returned empty transcript, playing error sound")
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END, []*api.VoiceAssistantEventData{
			{Name: "text", Value: ""},
		}); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "stt-no-text-recognized"},
			{Name: "message", Value: "No text recognized"},
		}); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
			{Name: "url", Value: errorURL},
		}); err != nil {
			return err
		}
		return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	log.Printf("transcript: %q", transcript)

	if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END, []*api.VoiceAssistantEventData{
		{Name: "text", Value: transcript},
	}); err != nil {
		return err
	}

	// Check shortcuts before the webhook. If a shortcut matches, skip the webhook entirely.
	if matched, err := tryShortcut(transcript); matched {
		if err != nil {
			log.Printf("shortcut error: %v", err)
			if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
				{Name: "code", Value: "pipeline-error"},
				{Name: "message", Value: err.Error()},
			}); err != nil {
				return err
			}
			if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil); err != nil {
				return err
			}
			if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
				{Name: "url", Value: errorURL},
			}); err != nil {
				return err
			}
			return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
		}
		// Shortcut succeeded: play the tone and end the run.
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_START, nil); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_END, nil); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
			{Name: "url", Value: ttsURL},
		}); err != nil {
			return err
		}
		return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	responseText, err := postWebhook(transcript)
	if err != nil {
		log.Printf("postWebhook error: %v", err)
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "pipeline-error"},
			{Name: "message", Value: err.Error()},
		}); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil); err != nil {
			return err
		}
		if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
			{Name: "url", Value: errorURL},
		}); err != nil {
			return err
		}
		return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	playbackURL := ttsURL
	if responseText != "" {
		audio, err := synthesizeSpeech(responseText)
		if err != nil {
			log.Printf("synthesizeSpeech error, falling back to tone.wav: %v", err)
		} else {
			ttsAudioMutex.Lock()
			ttsAudioBuffer = audio
			ttsAudioMutex.Unlock()
			playbackURL = ttsResponseURL
		}
	}

	if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_START, nil); err != nil {
		return err
	}
	if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_END, nil); err != nil {
		return err
	}
	if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, []*api.VoiceAssistantEventData{
		{Name: "text", Value: responseText},
	}); err != nil {
		return err
	}
	if err := sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
		{Name: "url", Value: playbackURL},
	}); err != nil {
		return err
	}
	return sendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
}

// synthesizeSpeech calls the ElevenLabs TTS API and returns the mp3 audio bytes.
func synthesizeSpeech(text string) ([]byte, error) {
	settingsMutex.RLock()
	speed := currentSettings.TtsSpeed
	settingsMutex.RUnlock()

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
	request.Header.Set("xi-api-key", elevenLabsAPIKey)
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

// sendEvent sends a VoiceAssistantEventResponse with the given event type and optional data.
func sendEvent(writer io.Writer, eventType api.VoiceAssistantEvent, data []*api.VoiceAssistantEventData) error {
	log.Printf("sending event %s", eventType)
	return sendMessage(writer, messageTypeVoiceAssistantEventResponse, &api.VoiceAssistantEventResponse{
		EventType: eventType,
		Data:      data,
	})
}
