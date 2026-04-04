package havpe

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// SaveSettings writes the given settings to the file at path.
func SaveSettings(path string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write settings file: %w", err)
	}
	return nil
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.settingsMu.RLock()
		data, err := json.Marshal(s.settings)
		s.settingsMu.RUnlock()
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

		s.settingsMu.Lock()
		candidate := s.settings
		if update.SttLanguage != nil {
			candidate.SttLanguage = *update.SttLanguage
		}
		if update.TtsSpeed != nil {
			candidate.TtsSpeed = *update.TtsSpeed
		}
		if candidate.SttLanguage == "" {
			s.settingsMu.Unlock()
			http.Error(w, "stt_language must not be empty", http.StatusBadRequest)
			return
		}
		if candidate.TtsSpeed <= 0 {
			s.settingsMu.Unlock()
			http.Error(w, "tts_speed must be positive", http.StatusBadRequest)
			return
		}
		s.settings = candidate
		snapshot := s.settings
		s.settingsMu.Unlock()

		if err := SaveSettings(s.settingsPath, snapshot); err != nil {
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
