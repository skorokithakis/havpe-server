package havpe

import "net/http"

func (s *Server) handleToneWav(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeFile(w, r, "tone.wav")
}

func (s *Server) handleErrorWav(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeFile(w, r, "error.wav")
}

func (s *Server) handleTTSMP3(w http.ResponseWriter, r *http.Request) {
	audio := s.audioBuffer.Get()
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Write(audio)
}
