package havpe

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/skorokithakis/havpe-server/pkg/esphome/api"
	"github.com/skorokithakis/havpe-server/pkg/elevenlabs"
	"github.com/skorokithakis/havpe-server/pkg/esphome"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

const (
	preSpeechTimeout   = 3 * time.Second
	audioCaptureWindow = 15 * time.Second
	sttTranscriptTimeout = 10 * time.Second
)

// pipelineState holds all mutable state for a single voice pipeline run.
type pipelineState struct {
	active                   bool
	startTime                time.Time
	audioBuffer              []byte
	vadStartSent             bool
	vadFrameBuffer           []byte
	speechDetected           bool
	consecutiveSpeechFrames  int
	consecutiveSilenceFrames int
	lastSpeechEndTime        time.Time
	sttConn                  *websocket.Conn
	transcriptChannel        chan string
}

// HandleVoiceAssistantRequest handles message type 90. It implements
// esphome.VoiceHandler.
func (s *Server) HandleVoiceAssistantRequest(writer io.Writer, data []byte) error {
	var request api.VoiceAssistantRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		log.Printf("unmarshal VoiceAssistantRequest: %v", err)
		s.pipeline = pipelineState{}
		return nil
	}

	if !request.GetStart() {
		log.Printf("VoiceAssistantRequest start=false: pipeline cancelled by device, resetting state")
		s.closeSTTConnection()
		s.pipeline = pipelineState{}
		return esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	log.Printf("VoiceAssistantRequest start=true: beginning pipeline run (wake_word=%q flags=%d)",
		request.GetWakeWordPhrase(), request.GetFlags())

	s.pipeline = pipelineState{
		active:            true,
		startTime:         time.Now(),
		transcriptChannel: make(chan string, 1),
	}

	if s.detector != nil {
		if err := s.detector.Reset(); err != nil {
			log.Printf("reset VAD detector: %v", err)
		}
	}

	if s.recordDir == "" && s.sttClient != nil {
		s.settingsMu.RLock()
		language := s.settings.SttLanguage
		s.settingsMu.RUnlock()

		sttConn, err := s.sttClient.Dial(language)
		if err != nil {
			log.Printf("open ElevenLabs STT WebSocket: %v", err)
			close(s.pipeline.transcriptChannel)
		} else {
			s.pipeline.sttConn = sttConn
			go elevenlabs.ReadSTTMessages(sttConn, s.pipeline.transcriptChannel)
		}
	}

	if err := esphome.SendMessage(writer, esphome.MessageTypeVoiceAssistantResponse, &api.VoiceAssistantResponse{Port: 0}); err != nil {
		return err
	}

	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_START, nil); err != nil {
		return err
	}
	return esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_START, nil)
}

// HandleVoiceAssistantAudio handles message type 106. It implements
// esphome.VoiceHandler.
func (s *Server) HandleVoiceAssistantAudio(writer io.Writer, data []byte) error {
	if !s.pipeline.active {
		log.Printf("audio chunk received but no active pipeline, ignoring")
		return nil
	}

	if s.recordDir != "" {
		return s.handleRecordingAudio(writer, data)
	}

	var chunk api.VoiceAssistantAudio
	if err := proto.Unmarshal(data, &chunk); err != nil {
		log.Printf("unmarshal VoiceAssistantAudio: %v", err)
		return nil
	}

	if !s.pipeline.vadStartSent {
		log.Printf("first audio chunk received, sending STT_VAD_START")
		if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_START, nil); err != nil {
			return err
		}
		s.pipeline.vadStartSent = true
	}

	chunkData := chunk.GetData()
	s.pipeline.audioBuffer = append(s.pipeline.audioBuffer, chunkData...)

	if s.pipeline.sttConn != nil {
		if !elevenlabs.SendSTTChunk(s.pipeline.sttConn, chunkData, false) {
			s.pipeline.sttConn = nil
		}
	}

	s.pipeline.vadFrameBuffer = append(s.pipeline.vadFrameBuffer, chunkData...)
	for len(s.pipeline.vadFrameBuffer) >= vadFrameSize {
		frame := s.pipeline.vadFrameBuffer[:vadFrameSize]
		s.pipeline.vadFrameBuffer = s.pipeline.vadFrameBuffer[vadFrameSize:]

		if s.detector == nil {
			continue
		}

		samples := make([]float32, vadFrameSize/2)
		for i := range samples {
			sample := int16(binary.LittleEndian.Uint16(frame[i*2 : i*2+2]))
			samples[i] = float32(sample) / 32768.0
		}

		probability, err := s.detector.Infer(samples)
		if err != nil {
			log.Printf("VAD infer error: %v", err)
			continue
		}

		if !s.pipeline.speechDetected {
			if probability >= speechStartThreshold {
				s.pipeline.consecutiveSpeechFrames++
				if s.pipeline.consecutiveSpeechFrames >= speechStartFramesRequired {
					s.pipeline.speechDetected = true
					s.pipeline.consecutiveSilenceFrames = 0
					log.Printf("speech start detected after %d consecutive frames", s.pipeline.consecutiveSpeechFrames)
				}
			} else {
				s.pipeline.consecutiveSpeechFrames = 0
			}
			continue
		}

		if probability >= speechContinueThreshold {
			s.pipeline.consecutiveSilenceFrames = 0
		} else {
			s.pipeline.consecutiveSilenceFrames++
			if s.pipeline.consecutiveSilenceFrames >= silenceFramesRequired {
				log.Printf("VAD end-of-speech detected after %d silence frames, finalising pipeline",
					s.pipeline.consecutiveSilenceFrames)
				s.pipeline.active = false
				if s.pipeline.sttConn != nil {
					if !elevenlabs.SendSTTChunk(s.pipeline.sttConn, nil, true) {
						s.pipeline.sttConn = nil
					}
				}
				return s.runPipelineResponse(writer)
			}
		}
	}

	elapsed := time.Since(s.pipeline.startTime)

	if !s.pipeline.speechDetected && elapsed >= preSpeechTimeout {
		log.Printf("no speech detected after %v, aborting pipeline", preSpeechTimeout)
		s.pipeline.active = false
		s.closeSTTConnection()
		if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
			{Name: "code", Value: "stt-no-text-recognized"},
			{Name: "message", Value: "No speech detected"},
		}); err != nil {
			return err
		}
		return esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
	}

	if elapsed >= audioCaptureWindow {
		log.Printf("audio capture window elapsed, finalising pipeline")
		s.pipeline.active = false
		if s.pipeline.sttConn != nil {
			if !elevenlabs.SendSTTChunk(s.pipeline.sttConn, nil, true) {
				s.pipeline.sttConn = nil
			}
		}
		return s.runPipelineResponse(writer)
	}

	return nil
}

func (s *Server) closeSTTConnection() {
	if s.pipeline.sttConn != nil {
		s.pipeline.sttConn.Close()
		s.pipeline.sttConn = nil
	}
}

func (s *Server) waitForTranscript() (string, error) {
	if s.pipeline.transcriptChannel == nil {
		return "", fmt.Errorf("no STT transcript channel (pipeline not initialized)")
	}
	select {
	case transcript, ok := <-s.pipeline.transcriptChannel:
		if !ok {
			return "", fmt.Errorf("STT WebSocket connection closed before transcript arrived")
		}
		return transcript, nil
	case <-time.After(sttTranscriptTimeout):
		return "", fmt.Errorf("timed out waiting for STT transcript after %v", sttTranscriptTimeout)
	}
}
