package main

import (
	"bytes"
	"net"
	"testing"
	"time"

	"havpe-server/api"

	"google.golang.org/protobuf/proto"
)

// nullConn is a net.Conn that discards all writes and returns EOF on reads.
// It is used in tests that exercise handler logic without a real network connection.
type nullConn struct{ bytes.Buffer }

func (nullConn) Close() error                       { return nil }
func (nullConn) LocalAddr() net.Addr                { return nil }
func (nullConn) RemoteAddr() net.Addr               { return nil }
func (nullConn) SetDeadline(_ time.Time) error      { return nil }
func (nullConn) SetReadDeadline(_ time.Time) error  { return nil }
func (nullConn) SetWriteDeadline(_ time.Time) error { return nil }

// marshalAudioChunk encodes a VoiceAssistantAudio protobuf message for use as raw frame data.
func marshalAudioChunk(t *testing.T, audioData []byte) []byte {
	t.Helper()
	data, err := proto.Marshal(&api.VoiceAssistantAudio{Data: audioData})
	if err != nil {
		t.Fatalf("marshal VoiceAssistantAudio: %v", err)
	}
	return data
}

// TestHandleVoiceAssistantAudio_inactivePipelineIgnored verifies that audio frames
// arriving when no pipeline is active are silently dropped.
func TestHandleVoiceAssistantAudio_inactivePipelineIgnored(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{active: false}

	handleVoiceAssistantAudio(conn, marshalAudioChunk(t, []byte{0x01, 0x02}), pipeline, "", "", "")

	if pipeline.vadStartSent {
		t.Error("vadStartSent should remain false when pipeline is inactive")
	}
	if len(pipeline.audioBuffer) != 0 {
		t.Errorf("audioBuffer should be empty when pipeline is inactive, got %d bytes", len(pipeline.audioBuffer))
	}
}

// TestHandleVoiceAssistantAudio_accumulatesWithinWindow verifies that audio chunks are
// appended to the buffer while the capture window has not yet elapsed.
func TestHandleVoiceAssistantAudio_accumulatesWithinWindow(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{
		active:    true,
		startTime: time.Now(), // window starts now, so it has not elapsed yet
	}

	chunk := []byte{0xAA, 0xBB, 0xCC}
	handleVoiceAssistantAudio(conn, marshalAudioChunk(t, chunk), pipeline, "", "", "")

	if !pipeline.active {
		t.Error("pipeline should still be active within the capture window")
	}
	if !bytes.Equal(pipeline.audioBuffer, chunk) {
		t.Errorf("audioBuffer: got %x, want %x", pipeline.audioBuffer, chunk)
	}
	if !pipeline.vadStartSent {
		t.Error("vadStartSent should be true after the first chunk")
	}
}

// TestHandleVoiceAssistantAudio_windowElapsedFinalises verifies that once
// audioCaptureWindow has elapsed the pipeline is deactivated and the response is sent.
// We back-date startTime so the window appears to have already elapsed.
func TestHandleVoiceAssistantAudio_windowElapsedFinalises(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{
		active:    true,
		startTime: time.Now().Add(-(audioCaptureWindow + time.Millisecond)),
	}

	chunk := []byte{0x01, 0x02}
	handleVoiceAssistantAudio(conn, marshalAudioChunk(t, chunk), pipeline, "", "", "")

	if pipeline.active {
		t.Error("pipeline should be inactive after the capture window elapses")
	}
}

// TestHandleVoiceAssistantAudio_stragglersIgnoredAfterFinalise verifies that audio
// frames arriving after the pipeline has been finalised are silently dropped.
func TestHandleVoiceAssistantAudio_stragglersIgnoredAfterFinalise(t *testing.T) {
	conn := &nullConn{}
	// Simulate a pipeline that was already finalised.
	pipeline := &pipelineState{active: false}

	handleVoiceAssistantAudio(conn, marshalAudioChunk(t, []byte{0xFF}), pipeline, "", "", "")

	if len(pipeline.audioBuffer) != 0 {
		t.Errorf("straggler audio should not be buffered, got %d bytes", len(pipeline.audioBuffer))
	}
}

// TestHandleVoiceAssistantRequest_startTrue verifies that a start=true request
// initialises the pipeline state correctly.
func TestHandleVoiceAssistantRequest_startTrue(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{}

	requestData, err := proto.Marshal(&api.VoiceAssistantRequest{Start: true})
	if err != nil {
		t.Fatalf("marshal VoiceAssistantRequest: %v", err)
	}

	before := time.Now()
	handleVoiceAssistantRequest(conn, requestData, pipeline)
	after := time.Now()

	if !pipeline.active {
		t.Error("pipeline should be active after start=true")
	}
	if pipeline.startTime.Before(before) || pipeline.startTime.After(after) {
		t.Errorf("startTime %v not in expected range [%v, %v]", pipeline.startTime, before, after)
	}
}

// TestHandleVoiceAssistantRequest_startFalse verifies that a start=false request
// resets the pipeline state.
func TestHandleVoiceAssistantRequest_startFalse(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{
		active:       true,
		vadStartSent: true,
		audioBuffer:  []byte{0x01},
	}

	requestData, err := proto.Marshal(&api.VoiceAssistantRequest{Start: false})
	if err != nil {
		t.Fatalf("marshal VoiceAssistantRequest: %v", err)
	}

	handleVoiceAssistantRequest(conn, requestData, pipeline)

	if pipeline.active {
		t.Error("pipeline should be inactive after start=false")
	}
	if len(pipeline.audioBuffer) != 0 {
		t.Errorf("audioBuffer should be cleared after start=false, got %d bytes", len(pipeline.audioBuffer))
	}
}

// makeZeroFrame returns a slice of n zero bytes, representing silence in 16-bit LE PCM.
func makeZeroFrame(n int) []byte {
	return make([]byte, n)
}

// TestHandleVoiceAssistantAudio_vadFrameBufferingCarriesRemainder verifies that bytes
// that don't fill a complete 1024-byte VAD window are held in vadFrameBuffer and not lost.
func TestHandleVoiceAssistantAudio_vadFrameBufferingCarriesRemainder(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{
		active:    true,
		startTime: time.Now(),
	}

	// Send 500 bytes — less than one full VAD window (1024 bytes).
	chunk := makeZeroFrame(500)
	handleVoiceAssistantAudio(conn, marshalAudioChunk(t, chunk), pipeline, "", "", "")

	if !pipeline.active {
		t.Error("pipeline should still be active: no full VAD window processed yet")
	}
	if len(pipeline.vadFrameBuffer) != 500 {
		t.Errorf("vadFrameBuffer: got %d bytes, want 500", len(pipeline.vadFrameBuffer))
	}
}

// TestHandleVoiceAssistantAudio_silenceBeforeSpeechDoesNotFinalise verifies that
// consecutive silence frames do not trigger end-of-speech before any speech is detected.
func TestHandleVoiceAssistantAudio_silenceBeforeSpeechDoesNotFinalise(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{
		active:    true,
		startTime: time.Now(),
	}

	// Send enough silence to trigger more than silenceFramesRequired Detect calls, but
	// no speech first. Each Detect call consumes vadFrameSize bytes.
	silenceData := makeZeroFrame(vadFrameSize * (silenceFramesRequired + 5))
	handleVoiceAssistantAudio(conn, marshalAudioChunk(t, silenceData), pipeline, "", "", "")

	if !pipeline.active {
		t.Error("pipeline should not be finalised by silence before any speech is detected")
	}
	if pipeline.consecutiveSilenceFrames != 0 {
		t.Errorf("consecutiveSilenceFrames should be 0 before speech detected, got %d",
			pipeline.consecutiveSilenceFrames)
	}
}

// TestHandleVoiceAssistantAudio_vadEndOfSpeechFinalises verifies that the pipeline is
// finalized when silenceFramesRequired consecutive silence frames follow detected speech.
// We manipulate the pipeline state directly to simulate prior speech detection, then
// send enough silence to cross the threshold.
func TestHandleVoiceAssistantAudio_vadEndOfSpeechFinalises(t *testing.T) {
	conn := &nullConn{}
	pipeline := &pipelineState{
		active:         true,
		startTime:      time.Now(),
		vadStartSent:   true,
		speechDetected: true,
		// Already at silenceFramesRequired-1 so one more Detect call tips it over.
		consecutiveSilenceFrames: silenceFramesRequired - 1,
	}

	// One complete VAD frame (vadFrameSize) of silence should push
	// consecutiveSilenceFrames to the threshold and finalise the pipeline.
	silenceData := makeZeroFrame(vadFrameSize)
	handleVoiceAssistantAudio(conn, marshalAudioChunk(t, silenceData), pipeline, "", "", "")

	if pipeline.active {
		t.Error("pipeline should be finalised after reaching the silence threshold")
	}
}

// TestBuildWAVHeader verifies that transcribeAudio builds a valid WAV in memory by
// checking the header structure. We use a mock HTTP server to avoid hitting the real
// ElevenLabs API — the test only cares about WAV construction, not the API call.
// Since transcribeAudio combines WAV building and API call, we test the WAV header
// indirectly by verifying the buildWAV helper produces correct output.
// For now, this test is removed since transcribeAudio is not easily unit-testable
// without extracting the WAV building into a separate function.
