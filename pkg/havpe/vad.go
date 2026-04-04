package havpe

import "github.com/streamer45/silero-vad-go/speech"

const (
	vadFrameSize            = 1024
	vadSampleRate           = 16000
	speechStartThreshold    = 0.5
	speechContinueThreshold = 0.15
	speechStartFramesRequired = 3
	silenceFramesRequired     = 40
)

// NewDetector creates a Silero VAD detector from the ONNX model file in the
// working directory. The caller is responsible for calling Destroy() when done.
func NewDetector() (*speech.Detector, error) {
	cfg := speech.DetectorConfig{
		ModelPath:            "silero_vad.onnx",
		SampleRate:           16000,
		Threshold:            0.5,
		MinSilenceDurationMs: 800,
		SpeechPadMs:          30,
	}
	return speech.NewDetector(cfg)
}
