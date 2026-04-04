package havpe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/skorokithakis/havpe-server/pkg/esphome/api"
	"github.com/skorokithakis/havpe-server/pkg/esphome"

	"google.golang.org/protobuf/proto"
)

const interUtteranceSilenceTimeout = 5 * time.Second

func (s *Server) handleRecordingAudio(writer io.Writer, data []byte) error {
	var chunk api.VoiceAssistantAudio
	if err := proto.Unmarshal(data, &chunk); err != nil {
		log.Printf("unmarshal VoiceAssistantAudio: %v", err)
		return nil
	}

	if !s.pipeline.vadStartSent {
		log.Printf("first audio chunk received in recording mode, sending STT_VAD_START")
		if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_START, nil); err != nil {
			return err
		}
		s.pipeline.vadStartSent = true
	}

	chunkData := chunk.GetData()
	s.pipeline.audioBuffer = append(s.pipeline.audioBuffer, chunkData...)

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
				log.Printf("VAD end-of-speech detected after %d silence frames", s.pipeline.consecutiveSilenceFrames)
				s.pipeline.lastSpeechEndTime = time.Now()
				s.pipeline.speechDetected = false
				s.pipeline.consecutiveSpeechFrames = 0
				s.pipeline.consecutiveSilenceFrames = 0
			}
		}
	}

	if !s.pipeline.speechDetected {
		referenceTime := s.pipeline.lastSpeechEndTime
		if referenceTime.IsZero() {
			referenceTime = s.pipeline.startTime
		}
		if time.Since(referenceTime) >= interUtteranceSilenceTimeout {
			log.Printf("inter-utterance silence timeout after %v, ending session", interUtteranceSilenceTimeout)
			if !s.pipeline.lastSpeechEndTime.IsZero() {
				filename := fmt.Sprintf("%03d.wav", s.recordingCounter+1)
				filePath := filepath.Join(s.recordDir, filename)
				if err := os.WriteFile(filePath, buildWAV(s.pipeline.audioBuffer), 0o644); err != nil {
					log.Printf("write WAV file %s: %v", filePath, err)
				} else {
					s.recordingCounter++
					log.Printf("saved session recording to %s", filePath)
				}
			}
			s.pipeline.active = false
			return esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
		}
	}

	return nil
}

func buildWAV(pcmData []byte) []byte {
	const (
		sampleRate    = 16000
		channels      = 1
		bitsPerSample = 16
		audioFormat   = 1
	)
	dataSize := uint32(len(pcmData))
	byteRate := uint32(sampleRate * channels * bitsPerSample / 8)
	blockAlign := uint16(channels * bitsPerSample / 8)

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
		ChunkSize:     36 + dataSize,
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
	_ = binary.Write(&wavBuffer, binary.LittleEndian, header)
	_, _ = wavBuffer.Write(pcmData)
	return wavBuffer.Bytes()
}
