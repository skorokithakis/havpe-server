package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"sync"
	"time"

	"havpe-server/api"

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
}

// httpPort is the port on which the HTTP server serves TTS audio files.
// Defined as a const so it is easy to change without hunting through the code.
const httpPort = 8085

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s <device-host>", os.Args[0])
	}

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

	host := os.Args[1]
	address := host + ":6053"

	log.Printf("connecting to %s", address)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		log.Fatalf("dial %s: %v", address, err)
	}
	log.Printf("connected to %s", address)

	localIP := conn.LocalAddr().(*net.TCPAddr).IP.String()
	ttsURL := fmt.Sprintf("http://%s:%d/tone.wav", localIP, httpPort)
	errorURL := fmt.Sprintf("http://%s:%d/error.wav", localIP, httpPort)
	ttsResponseURL := fmt.Sprintf("http://%s:%d/tts.mp3", localIP, httpPort)
	log.Printf("TTS tone URL: %s", ttsURL)
	log.Printf("TTS error URL: %s", errorURL)
	log.Printf("TTS response URL: %s", ttsResponseURL)

	handleConnection(conn, ttsURL, errorURL, ttsResponseURL)
}

func handleConnection(conn net.Conn, ttsURL string, errorURL string, ttsResponseURL string) {
	defer conn.Close()

	// bufio.Reader is required so that ReadFrame can use it as an io.ByteReader
	// for varint decoding without wrapping it again internally.
	reader := bufio.NewReader(conn)

	// Perform the handshake before entering the read loop. The device may send
	// PingRequest at any point during the handshake; readHandshakeResponse handles
	// those transparently.

	// Step 1: client sends HelloRequest to identify itself.
	sendMessage(conn, messageTypeHelloRequest, &api.HelloRequest{
		ClientInfo:      "esphome-go-server",
		ApiVersionMajor: 1,
		ApiVersionMinor: 10,
	})

	// Step 2: device replies with HelloResponse.
	helloData, err := readHandshakeResponse(conn, reader, messageTypeHelloResponse)
	if err != nil {
		log.Fatalf("hello handshake: %v", err)
	}
	var helloResponse api.HelloResponse
	if err := proto.Unmarshal(helloData, &helloResponse); err != nil {
		log.Fatalf("unmarshal HelloResponse: %v", err)
	}
	log.Printf("HelloResponse: server_info=%q name=%q api_version=%d.%d",
		helloResponse.GetServerInfo(), helloResponse.GetName(),
		helloResponse.GetApiVersionMajor(), helloResponse.GetApiVersionMinor())

	// Step 3: client requests device info.
	// Authentication (message IDs 3-4) was removed in ESPHome 2026.1.0; sending
	// AuthenticationRequest causes the handshake to hang because the device never
	// replies with AuthenticationResponse.
	sendMessage(conn, messageTypeDeviceInfoRequest, &api.DeviceInfoRequest{})

	// Step 4: device replies with DeviceInfoResponse.
	deviceInfoData, err := readHandshakeResponse(conn, reader, messageTypeDeviceInfoResponse)
	if err != nil {
		log.Fatalf("device info handshake: %v", err)
	}
	var deviceInfoResponse api.DeviceInfoResponse
	if err := proto.Unmarshal(deviceInfoData, &deviceInfoResponse); err != nil {
		log.Fatalf("unmarshal DeviceInfoResponse: %v", err)
	}
	log.Printf("DeviceInfoResponse: name=%q model=%q", deviceInfoResponse.GetName(), deviceInfoResponse.GetModel())

	// Step 5: client requests entity list; device sends multiple ListEntities* messages
	// followed by ListEntitiesDoneResponse (ID 19).
	sendMessage(conn, messageTypeListEntitiesRequest, &api.ListEntitiesRequest{})
	if err := drainEntityList(conn, reader); err != nil {
		log.Fatalf("entity list: %v", err)
	}

	// Step 6: subscribe to state updates and voice assistant events.
	sendMessage(conn, messageTypeSubscribeStatesRequest, &api.SubscribeStatesRequest{})
	// flags=1 requests API audio streaming (audio sent over the API connection, not UDP).
	sendMessage(conn, messageTypeSubscribeVoiceAssistantRequest, &api.SubscribeVoiceAssistantRequest{
		Subscribe: true,
		Flags:     1,
	})

	var pipeline pipelineState

	for {
		messageType, data, err := ReadFrame(reader)
		if err != nil {
			if err == io.EOF {
				log.Println("connection closed by device")
			} else {
				log.Printf("read frame: %v", err)
			}
			return
		}

		switch messageType {
		case messageTypePingRequest:
			handlePingRequest(conn)
		case messageTypeDisconnectRequest:
			handleDisconnectRequest(conn)
			return
		case messageTypeVoiceAssistantRequest:
			handleVoiceAssistantRequest(conn, data, &pipeline)
		case messageTypeVoiceAssistantAudio:
			handleVoiceAssistantAudio(conn, data, &pipeline, ttsURL, errorURL, ttsResponseURL)
		default:
			log.Printf("ignoring message type %d", messageType)
		}
	}
}

// readHandshakeResponse reads frames from reader until a frame with the expected message
// type is received. It returns the body bytes of that frame.
//
// PingRequest frames are answered transparently. Any other unexpected frame type is logged
// and skipped rather than treated as an error, because the device may send state updates or
// service requests at any point during the handshake.
func readHandshakeResponse(conn net.Conn, reader *bufio.Reader, expectedType uint32) ([]byte, error) {
	for {
		messageType, data, err := ReadFrame(reader)
		if err != nil {
			return nil, fmt.Errorf("reading frame (expected type %d): %w", expectedType, err)
		}
		if messageType == messageTypePingRequest {
			handlePingRequest(conn)
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
func drainEntityList(conn net.Conn, reader *bufio.Reader) error {
	for {
		messageType, _, err := ReadFrame(reader)
		if err != nil {
			return fmt.Errorf("reading entity list frame: %w", err)
		}
		if messageType == messageTypePingRequest {
			handlePingRequest(conn)
			continue
		}
		if messageType == messageTypeListEntitiesDoneResponse {
			log.Printf("ListEntitiesDoneResponse received")
			return nil
		}
		log.Printf("ignoring entity list message type %d", messageType)
	}
}

func sendMessage(conn net.Conn, messageType uint32, message proto.Message) {
	data, err := proto.Marshal(message)
	if err != nil {
		log.Printf("marshal message type %d: %v", messageType, err)
		return
	}
	if err := WriteFrame(conn, messageType, data); err != nil {
		log.Printf("write frame type %d: %v", messageType, err)
	}
}

func handlePingRequest(conn net.Conn) {
	log.Printf("PingRequest received, sending PingResponse")
	sendMessage(conn, messageTypePingResponse, &api.PingResponse{})
}

func handleDisconnectRequest(conn net.Conn) {
	log.Printf("DisconnectRequest received, sending DisconnectResponse and closing")
	sendMessage(conn, messageTypeDisconnectResponse, &api.DisconnectResponse{})
}

// handleVoiceAssistantRequest handles message type 90. When start=true it begins a new
// pipeline run: resets the pipeline state, records the start time, sends
// VoiceAssistantResponse with port=0 (API audio mode), and sends the RUN_START and
// STT_START events. When start=false the device has cancelled the run; we just reset state.
func handleVoiceAssistantRequest(conn net.Conn, data []byte, pipeline *pipelineState) {
	var request api.VoiceAssistantRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		log.Printf("unmarshal VoiceAssistantRequest: %v", err)
		*pipeline = pipelineState{}
		return
	}

	if !request.GetStart() {
		log.Printf("VoiceAssistantRequest start=false: pipeline cancelled by device, resetting state")
		*pipeline = pipelineState{}
		// The device may be stuck in AWAITING_RESPONSE or another non-idle state after
		// sending start=false. Sending RUN_END ensures its state machine reaches IDLE and
		// the on_end trigger fires to reset LEDs.
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
		return
	}

	log.Printf("VoiceAssistantRequest start=true: beginning pipeline run (wake_word=%q flags=%d)",
		request.GetWakeWordPhrase(), request.GetFlags())

	*pipeline = pipelineState{
		active:    true,
		startTime: time.Now(),
	}

	// Reset the VAD detector state so that state from a previous pipeline run does not
	// bleed into the new one.
	if err := detector.Reset(); err != nil {
		log.Printf("reset VAD detector: %v", err)
	}

	// port=0 tells the device to stream audio over the API connection rather than UDP.
	sendMessage(conn, messageTypeVoiceAssistantResponse, &api.VoiceAssistantResponse{Port: 0})

	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_START, nil)
	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_START, nil)
}

// handleVoiceAssistantAudio handles message type 106. It accumulates PCM chunks into
// pipeline.audioBuffer while the pipeline is active. Each incoming chunk is fed through
// the Silero VAD in 1024-byte (32ms, 512-sample) windows to detect end-of-speech. The
// pipeline is finalized when 800ms of consecutive silence follows detected speech, or
// when the hard audioCaptureWindow maximum is reached. Audio frames arriving when the
// pipeline is inactive are silently ignored.
func handleVoiceAssistantAudio(conn net.Conn, data []byte, pipeline *pipelineState, ttsURL string, errorURL string, ttsResponseURL string) {
	if !pipeline.active {
		log.Printf("audio chunk received but no active pipeline, ignoring")
		return
	}

	var chunk api.VoiceAssistantAudio
	if err := proto.Unmarshal(data, &chunk); err != nil {
		log.Printf("unmarshal VoiceAssistantAudio: %v", err)
		return
	}

	if !pipeline.vadStartSent {
		log.Printf("first audio chunk received, sending STT_VAD_START")
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_START, nil)
		pipeline.vadStartSent = true
	}

	chunkData := chunk.GetData()
	pipeline.audioBuffer = append(pipeline.audioBuffer, chunkData...)
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
				runPipelineResponse(conn, pipeline.audioBuffer, ttsURL, errorURL, ttsResponseURL)
				return
			}
		}
	}

	elapsed := time.Since(pipeline.startTime)

	if !pipeline.speechDetected && elapsed >= preSpeechTimeout {
		log.Printf("no speech detected after %v, aborting pipeline", preSpeechTimeout)
		pipeline.active = false
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "stt-no-text-recognized"},
			{Name: "message", Value: "No speech detected"},
		})
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
		return
	}

	if elapsed >= audioCaptureWindow {
		// Hard safety cap: finalize even if VAD never triggered end-of-speech.
		log.Printf("audio capture window elapsed, finalising pipeline")
		pipeline.active = false
		runPipelineResponse(conn, pipeline.audioBuffer, ttsURL, errorURL, ttsResponseURL)
	}
}

// transcribeAudio builds a WAV file in memory from the raw PCM audio buffer and sends it
// to the ElevenLabs speech-to-text API. It returns the transcript text on success.
func transcribeAudio(audioBuffer []byte) (string, error) {
	const (
		sampleRate    = 16000
		channels      = 1
		bitsPerSample = 16
		audioFormat   = 1 // PCM
	)
	dataSize := uint32(len(audioBuffer))
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
	if err := binary.Write(&wavBuffer, binary.LittleEndian, header); err != nil {
		return "", fmt.Errorf("write WAV header: %w", err)
	}
	if _, err := wavBuffer.Write(audioBuffer); err != nil {
		return "", fmt.Errorf("write PCM data: %w", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("model_id", "scribe_v2"); err != nil {
		return "", fmt.Errorf("write model_id field: %w", err)
	}
	if err := writer.WriteField("language_code", "en"); err != nil {
		return "", fmt.Errorf("write language_code field: %w", err)
	}

	// The file part requires explicit Content-Type because multipart.Writer defaults to
	// application/octet-stream, which ElevenLabs may reject.
	fileHeader := make(textproto.MIMEHeader)
	fileHeader.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	fileHeader.Set("Content-Type", "audio/wav")
	filePart, err := writer.CreatePart(fileHeader)
	if err != nil {
		return "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := filePart.Write(wavBuffer.Bytes()); err != nil {
		return "", fmt.Errorf("write WAV to multipart: %w", err)
	}

	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	request, err := http.NewRequest("POST", "https://api.elevenlabs.io/v1/speech-to-text", &body)
	if err != nil {
		return "", fmt.Errorf("create STT request: %w", err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("xi-api-key", elevenLabsAPIKey)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("STT request: %w", err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read STT response body: %w", err)
	}

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("STT API returned status %d: %s", response.StatusCode, responseBody)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("parse STT response: %w", err)
	}

	return result.Text, nil
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

// runPipelineResponse transcribes the audio, posts the transcript to the webhook, and
// sends the remaining pipeline events to the device. When the webhook returns a non-empty
// response text, it is synthesized to speech and the device plays ttsResponseURL; otherwise
// the device plays ttsURL (tone.wav). On any failure it plays errorURL with a VOICE_ASSISTANT_ERROR event.
func runPipelineResponse(conn net.Conn, audioBuffer []byte, ttsURL string, errorURL string, ttsResponseURL string) {
	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_END, nil)

	transcript, err := transcribeAudio(audioBuffer)
	if err != nil {
		log.Printf("transcribeAudio error: %v", err)
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "pipeline-error"},
			{Name: "message", Value: err.Error()},
		})
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil)
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
			{Name: "url", Value: errorURL},
		})
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
		return
	}

	log.Printf("transcript: %q", transcript)

	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END, []*api.VoiceAssistantEventData{
		{Name: "text", Value: transcript},
	})

	responseText, err := postWebhook(transcript)
	if err != nil {
		log.Printf("postWebhook error: %v", err)
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "pipeline-error"},
			{Name: "message", Value: err.Error()},
		})
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil)
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
			{Name: "url", Value: errorURL},
		})
		sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
		return
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

	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_START, nil)
	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_END, nil)
	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, []*api.VoiceAssistantEventData{
		{Name: "text", Value: responseText},
	})
	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
		{Name: "url", Value: playbackURL},
	})
	sendEvent(conn, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
}

// synthesizeSpeech calls the ElevenLabs TTS API and returns the mp3 audio bytes.
func synthesizeSpeech(text string) ([]byte, error) {
	payload := map[string]string{
		"text":     text,
		"model_id": "eleven_v3",
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal TTS payload: %w", err)
	}

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
func sendEvent(conn net.Conn, eventType api.VoiceAssistantEvent, data []*api.VoiceAssistantEventData) {
	log.Printf("sending event %s", eventType)
	sendMessage(conn, messageTypeVoiceAssistantEventResponse, &api.VoiceAssistantEventResponse{
		EventType: eventType,
		Data:      data,
	})
}
