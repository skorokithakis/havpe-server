package havpe

import (
	"io"
	"log"
	"strings"

	"github.com/skorokithakis/havpe-server/pkg/esphome"
	"github.com/skorokithakis/havpe-server/pkg/esphome/api"
	"github.com/skorokithakis/havpe-server/pkg/processors"
)

func (s *Server) runPipelineResponse(writer io.Writer) error {
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_VAD_END, nil); err != nil {
		return err
	}

	transcript, transcriptErr := s.waitForTranscript()
	s.closeSTTConnection()

	if transcriptErr != nil {
		log.Printf("STT error: %v", transcriptErr)
		return s.sendErrorAndEnd(writer, "pipeline-error", transcriptErr.Error())
	}

	if strings.TrimSpace(transcript) == "" {
		log.Printf("STT returned empty transcript, playing error sound")
		if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END, []*api.VoiceAssistantEventData{
			{Name: "text", Value: ""},
		}); err != nil {
			return err
		}
		return s.sendErrorAndEnd(writer, "stt-no-text-recognized", "No text recognized")
	}

	log.Printf("transcript: %q", transcript)

	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_STT_END, []*api.VoiceAssistantEventData{
		{Name: "text", Value: transcript},
	}); err != nil {
		return err
	}

	req := &processors.TranscriptRequest{Transcript: transcript}
	resp := &processors.TranscriptResponse{}

	for _, p := range s.processors {
		if err := p.Process(req, resp); err != nil {
			log.Printf("processor error: %v", err)
			return s.sendErrorAndEnd(writer, "pipeline-error", err.Error())
		}
		if resp.StopProcessing {
			break
		}
	}

	playbackURL := s.ToneURL
	if resp.ResponseText != "" {
		s.settingsMu.RLock()
		speed := s.settings.TtsSpeed
		s.settingsMu.RUnlock()

		audio, err := s.ttsClient.SynthesizeSpeech(resp.ResponseText, speed)
		if err != nil {
			log.Printf("synthesizeSpeech error, falling back to tone.wav: %v", err)
		} else {
			s.audioBuffer.Set(audio)
			playbackURL = s.TTSResponseURL
		}
	}

	return s.sendIntentAndTTS(writer, resp.ResponseText, playbackURL)
}

func (s *Server) sendErrorAndEnd(writer io.Writer, code, message string) error {
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_ERROR, []*api.VoiceAssistantEventData{
		{Name: "code", Value: code},
		{Name: "message", Value: message},
	}); err != nil {
		return err
	}
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, nil); err != nil {
		return err
	}
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
		{Name: "url", Value: s.ErrorURL},
	}); err != nil {
		return err
	}
	return esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
}

func (s *Server) sendIntentAndTTS(writer io.Writer, responseText, playbackURL string) error {
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_START, nil); err != nil {
		return err
	}
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_INTENT_END, nil); err != nil {
		return err
	}
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_START, []*api.VoiceAssistantEventData{
		{Name: "text", Value: responseText},
	}); err != nil {
		return err
	}
	if err := esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_TTS_END, []*api.VoiceAssistantEventData{
		{Name: "url", Value: playbackURL},
	}); err != nil {
		return err
	}
	return esphome.SendEvent(writer, api.VoiceAssistantEvent_VOICE_ASSISTANT_RUN_END, nil)
}
