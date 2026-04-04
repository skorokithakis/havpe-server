package havpe

import "net/http"

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	_, password, ok := r.BasicAuth()
	if !ok || password != s.apiPassword {
		w.Header().Set("WWW-Authenticate", `Basic realm="havpe"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}
