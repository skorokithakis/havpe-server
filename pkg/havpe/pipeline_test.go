package havpe

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/skorokithakis/havpe-server/pkg/esphome/api"

	"google.golang.org/protobuf/proto"
)

type nullConn struct{ bytes.Buffer }

func (nullConn) Close() error                       { return nil }
func (nullConn) LocalAddr() net.Addr                { return nil }
func (nullConn) RemoteAddr() net.Addr               { return nil }
func (nullConn) SetDeadline(_ time.Time) error      { return nil }
func (nullConn) SetReadDeadline(_ time.Time) error  { return nil }
func (nullConn) SetWriteDeadline(_ time.Time) error { return nil }

func marshalAudioChunk(t *testing.T, audioData []byte) []byte {
	t.Helper()
	data, err := proto.Marshal(&api.VoiceAssistantAudio{Data: audioData})
	if err != nil {
		t.Fatalf("marshal VoiceAssistantAudio: %v", err)
	}
	return data
}

func TestHandleVoiceAssistantAudio_inactivePipelineIgnored(t *testing.T) {
	conn := &nullConn{}
	s := &Server{pipeline: pipelineState{active: false}}

	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, []byte{0x01, 0x02}))

	if s.pipeline.vadStartSent {
		t.Error("vadStartSent should remain false when pipeline is inactive")
	}
	if len(s.pipeline.audioBuffer) != 0 {
		t.Errorf("audioBuffer should be empty when pipeline is inactive, got %d bytes", len(s.pipeline.audioBuffer))
	}
}

func TestHandleVoiceAssistantAudio_accumulatesWithinWindow(t *testing.T) {
	conn := &nullConn{}
	s := &Server{
		pipeline: pipelineState{
			active:    true,
			startTime: time.Now(),
		},
	}

	chunk := []byte{0xAA, 0xBB, 0xCC}
	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, chunk))

	if !s.pipeline.active {
		t.Error("pipeline should still be active within the capture window")
	}
	if !bytes.Equal(s.pipeline.audioBuffer, chunk) {
		t.Errorf("audioBuffer: got %x, want %x", s.pipeline.audioBuffer, chunk)
	}
	if !s.pipeline.vadStartSent {
		t.Error("vadStartSent should be true after the first chunk")
	}
}

func TestHandleVoiceAssistantAudio_windowElapsedFinalises(t *testing.T) {
	conn := &nullConn{}
	s := &Server{
		pipeline: pipelineState{
			active:    true,
			startTime: time.Now().Add(-(audioCaptureWindow + time.Millisecond)),
		},
	}

	chunk := []byte{0x01, 0x02}
	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, chunk))

	if s.pipeline.active {
		t.Error("pipeline should be inactive after the capture window elapses")
	}
}

func TestHandleVoiceAssistantAudio_stragglersIgnoredAfterFinalise(t *testing.T) {
	conn := &nullConn{}
	s := &Server{pipeline: pipelineState{active: false}}

	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, []byte{0xFF}))

	if len(s.pipeline.audioBuffer) != 0 {
		t.Errorf("straggler audio should not be buffered, got %d bytes", len(s.pipeline.audioBuffer))
	}
}

func TestHandleVoiceAssistantRequest_startTrue(t *testing.T) {
	conn := &nullConn{}
	s := &Server{}

	requestData, err := proto.Marshal(&api.VoiceAssistantRequest{Start: true})
	if err != nil {
		t.Fatalf("marshal VoiceAssistantRequest: %v", err)
	}

	before := time.Now()
	s.HandleVoiceAssistantRequest(conn, requestData)
	after := time.Now()

	if !s.pipeline.active {
		t.Error("pipeline should be active after start=true")
	}
	if s.pipeline.startTime.Before(before) || s.pipeline.startTime.After(after) {
		t.Errorf("startTime %v not in expected range [%v, %v]", s.pipeline.startTime, before, after)
	}
}

func TestHandleVoiceAssistantRequest_startFalse(t *testing.T) {
	conn := &nullConn{}
	s := &Server{
		pipeline: pipelineState{
			active:       true,
			vadStartSent: true,
			audioBuffer:  []byte{0x01},
		},
	}

	requestData, err := proto.Marshal(&api.VoiceAssistantRequest{Start: false})
	if err != nil {
		t.Fatalf("marshal VoiceAssistantRequest: %v", err)
	}

	s.HandleVoiceAssistantRequest(conn, requestData)

	if s.pipeline.active {
		t.Error("pipeline should be inactive after start=false")
	}
	if len(s.pipeline.audioBuffer) != 0 {
		t.Errorf("audioBuffer should be cleared after start=false, got %d bytes", len(s.pipeline.audioBuffer))
	}
}

func makeZeroFrame(n int) []byte {
	return make([]byte, n)
}

func TestHandleVoiceAssistantAudio_vadFrameBufferingCarriesRemainder(t *testing.T) {
	conn := &nullConn{}
	s := &Server{
		pipeline: pipelineState{
			active:    true,
			startTime: time.Now(),
		},
	}

	chunk := makeZeroFrame(500)
	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, chunk))

	if !s.pipeline.active {
		t.Error("pipeline should still be active: no full VAD window processed yet")
	}
	if len(s.pipeline.vadFrameBuffer) != 500 {
		t.Errorf("vadFrameBuffer: got %d bytes, want 500", len(s.pipeline.vadFrameBuffer))
	}
}

func TestHandleVoiceAssistantAudio_silenceBeforeSpeechDoesNotFinalise(t *testing.T) {
	det, err := NewDetector()
	if err != nil {
		t.Skipf("skipping: VAD model not available: %v", err)
	}
	defer det.Destroy()

	conn := &nullConn{}
	s := &Server{
		detector: det,
		pipeline: pipelineState{
			active:    true,
			startTime: time.Now(),
		},
	}

	silenceData := makeZeroFrame(vadFrameSize * (silenceFramesRequired + 5))
	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, silenceData))

	if !s.pipeline.active {
		t.Error("pipeline should not be finalised by silence before any speech is detected")
	}
	if s.pipeline.consecutiveSilenceFrames != 0 {
		t.Errorf("consecutiveSilenceFrames should be 0 before speech detected, got %d",
			s.pipeline.consecutiveSilenceFrames)
	}
}

func TestHandleVoiceAssistantAudio_vadEndOfSpeechFinalises(t *testing.T) {
	det, err := NewDetector()
	if err != nil {
		t.Skipf("skipping: VAD model not available: %v", err)
	}
	defer det.Destroy()

	conn := &nullConn{}
	s := &Server{
		detector: det,
		pipeline: pipelineState{
			active:                   true,
			startTime:                time.Now(),
			vadStartSent:             true,
			speechDetected:           true,
			consecutiveSilenceFrames: silenceFramesRequired - 1,
		},
	}

	silenceData := makeZeroFrame(vadFrameSize)
	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, silenceData))

	if s.pipeline.active {
		t.Error("pipeline should be finalised after reaching the silence threshold")
	}
}

func TestBuildWAV(t *testing.T) {
	pcm := make([]byte, 3200)
	for i := range pcm {
		pcm[i] = byte(i % 256)
	}

	wav := buildWAV(pcm)

	if len(wav) != 44+len(pcm) {
		t.Fatalf("WAV length: got %d, want %d", len(wav), 44+len(pcm))
	}
	if string(wav[0:4]) != "RIFF" {
		t.Errorf("ChunkID: got %q, want %q", wav[0:4], "RIFF")
	}
	if string(wav[8:12]) != "WAVE" {
		t.Errorf("Format: got %q, want %q", wav[8:12], "WAVE")
	}
	if string(wav[12:16]) != "fmt " {
		t.Errorf("Subchunk1ID: got %q, want %q", wav[12:16], "fmt ")
	}
	if string(wav[36:40]) != "data" {
		t.Errorf("Subchunk2ID: got %q, want %q", wav[36:40], "data")
	}
	if !bytes.Equal(wav[44:], pcm) {
		t.Error("PCM data in WAV does not match input")
	}
}

func TestHandleRecordingAudio_interUtteranceSilenceTimeout(t *testing.T) {
	conn := &nullConn{}
	s := &Server{
		recordDir: t.TempDir(),
		pipeline: pipelineState{
			active:    true,
			startTime: time.Now().Add(-(interUtteranceSilenceTimeout + time.Millisecond)),
		},
	}

	chunk := makeZeroFrame(32)
	s.handleRecordingAudio(conn, marshalAudioChunk(t, chunk))

	if s.pipeline.active {
		t.Error("pipeline should be inactive after inter-utterance silence timeout")
	}
}

func TestHandleRecordingAudio_inactivePipelineIgnoredByParent(t *testing.T) {
	conn := &nullConn{}
	s := &Server{
		recordDir: "/tmp",
		pipeline:  pipelineState{active: false},
	}

	s.HandleVoiceAssistantAudio(conn, marshalAudioChunk(t, []byte{0x01, 0x02}))

	if len(s.pipeline.audioBuffer) != 0 {
		t.Errorf("audioBuffer should be empty when pipeline is inactive, got %d bytes", len(s.pipeline.audioBuffer))
	}
}
