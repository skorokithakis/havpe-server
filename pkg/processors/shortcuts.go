package processors

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type shortcut struct {
	Pattern    *regexp.Regexp
	rawPattern string
	URL        string
}

// ShortcutsProcessor matches transcripts against regex patterns and fires
// webhook POSTs. It also exposes CRUD HTTP handlers for managing shortcuts.
type ShortcutsProcessor struct {
	filePath    string
	apiPassword string
	shortcuts   []shortcut
	mu          sync.RWMutex
}

// NewShortcutsProcessor creates a ShortcutsProcessor. If the file at path does
// not exist, an empty one is created. apiPassword is used for Basic Auth on
// the CRUD endpoints.
func NewShortcutsProcessor(path, apiPassword string) (*ShortcutsProcessor, error) {
	p := &ShortcutsProcessor{
		filePath:    path,
		apiPassword: apiPassword,
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte("[]\n"), 0o644); err != nil {
			return nil, fmt.Errorf("create shortcuts file %s: %w", path, err)
		}
		log.Printf("created empty shortcuts file: %s", path)
	} else {
		loaded, err := loadShortcuts(path)
		if err != nil {
			return nil, fmt.Errorf("load shortcuts from %s: %w", path, err)
		}
		p.shortcuts = loaded
		log.Printf("loaded %d shortcut(s) from %s", len(loaded), path)
	}

	return p, nil
}

// Process implements TranscriptProcessor. The first matching shortcut wins:
// it POSTs to the shortcut URL and stops the chain (no TTS response text).
// Returns (nil, nil) when no shortcut matches.
func (p *ShortcutsProcessor) Process(req *TranscriptRequest) (*TranscriptResponse, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, sc := range p.shortcuts {
		if !sc.Pattern.MatchString(req.Transcript) {
			continue
		}
		log.Printf("shortcut matched %q -> %s", sc.Pattern, sc.URL)
		response, err := http.Post(sc.URL, "", nil)
		if err != nil {
			return nil, fmt.Errorf("shortcut POST to %s: %w", sc.URL, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("shortcut POST to %s returned status %d", sc.URL, response.StatusCode)
		}
		return &TranscriptResponse{StopProcessing: true}, nil
	}
	return nil, nil
}

// RegisterRoutes registers the shortcuts CRUD HTTP handlers on the given mux.
func (p *ShortcutsProcessor) RegisterRoutes(mux *http.ServeMux) {
	if mux == nil {
		mux = http.DefaultServeMux
	}
	mux.HandleFunc("/shortcuts/", p.handleShortcutsWithIndex)
	mux.HandleFunc("/shortcuts", p.handleShortcuts)
	log.Printf("shortcuts CRUD API enabled at /shortcuts")
}

func (p *ShortcutsProcessor) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	_, password, ok := r.BasicAuth()
	if !ok || password != p.apiPassword {
		w.Header().Set("WWW-Authenticate", `Basic realm="havpe"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (p *ShortcutsProcessor) handleShortcuts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.handleGetShortcuts(w, r)
	case http.MethodPost:
		p.handlePostShortcuts(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *ShortcutsProcessor) handleShortcutsWithIndex(w http.ResponseWriter, r *http.Request) {
	indexStr := strings.TrimPrefix(r.URL.Path, "/shortcuts/")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		http.Error(w, "invalid index", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		p.handlePutShortcut(w, r, index)
	case http.MethodDelete:
		p.handleDeleteShortcut(w, r, index)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *ShortcutsProcessor) handleGetShortcuts(w http.ResponseWriter, r *http.Request) {
	if !p.requireAuth(w, r) {
		return
	}

	p.mu.Lock()
	loaded, err := loadShortcuts(p.filePath)
	if err != nil {
		p.mu.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	p.shortcuts = loaded
	list := loaded
	p.mu.Unlock()

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (p *ShortcutsProcessor) handlePostShortcuts(w http.ResponseWriter, r *http.Request) {
	if !p.requireAuth(w, r) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var pair [2]string
	if err := json.Unmarshal(body, &pair); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	compiled, cleaned, err := compileShortcutPattern(pair[0])
	if err != nil {
		http.Error(w, "invalid regex: "+err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	loaded, err := loadShortcuts(p.filePath)
	if err != nil {
		p.mu.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	newIndex := len(loaded)
	loaded = append(loaded, shortcut{Pattern: compiled, rawPattern: cleaned, URL: pair[1]})
	if err := saveShortcuts(p.filePath, loaded); err != nil {
		p.mu.Unlock()
		http.Error(w, "save shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	p.shortcuts = loaded
	list := loaded
	p.mu.Unlock()

	log.Printf("added shortcut at index %d with regex %q", newIndex, cleaned)

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

func (p *ShortcutsProcessor) handlePutShortcut(w http.ResponseWriter, r *http.Request, index int) {
	if !p.requireAuth(w, r) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var pair [2]string
	if err := json.Unmarshal(body, &pair); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	compiled, cleaned, err := compileShortcutPattern(pair[0])
	if err != nil {
		http.Error(w, "invalid regex: "+err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	loaded, err := loadShortcuts(p.filePath)
	if err != nil {
		p.mu.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if index < 0 || index >= len(loaded) {
		p.mu.Unlock()
		http.Error(w, "index out of range", http.StatusNotFound)
		return
	}
	loaded[index] = shortcut{Pattern: compiled, rawPattern: cleaned, URL: pair[1]}
	if err := saveShortcuts(p.filePath, loaded); err != nil {
		p.mu.Unlock()
		http.Error(w, "save shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	p.shortcuts = loaded
	list := loaded
	p.mu.Unlock()

	log.Printf("updated shortcut at index %d with regex %q", index, cleaned)

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (p *ShortcutsProcessor) handleDeleteShortcut(w http.ResponseWriter, r *http.Request, index int) {
	if !p.requireAuth(w, r) {
		return
	}

	p.mu.Lock()
	loaded, err := loadShortcuts(p.filePath)
	if err != nil {
		p.mu.Unlock()
		http.Error(w, "load shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if index < 0 || index >= len(loaded) {
		p.mu.Unlock()
		http.Error(w, "index out of range", http.StatusNotFound)
		return
	}
	oldPattern := loaded[index].rawPattern
	loaded = append(loaded[:index], loaded[index+1:]...)
	if err := saveShortcuts(p.filePath, loaded); err != nil {
		p.mu.Unlock()
		http.Error(w, "save shortcuts: "+err.Error(), http.StatusInternalServerError)
		return
	}
	p.shortcuts = loaded
	list := loaded
	p.mu.Unlock()

	log.Printf("deleted shortcut at index %d with regex %q", index, oldPattern)

	data, err := shortcutsAsJSON(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func compileShortcutPattern(raw string) (*regexp.Regexp, string, error) {
	cleaned := raw
	for strings.HasPrefix(cleaned, "(?i)") {
		cleaned = strings.TrimPrefix(cleaned, "(?i)")
	}
	compiled, err := regexp.Compile("(?i)" + cleaned)
	if err != nil {
		return nil, "", err
	}
	return compiled, cleaned, nil
}

func loadShortcuts(path string) ([]shortcut, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read shortcuts file: %w", err)
	}

	var raw [][2]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse shortcuts JSON: %w", err)
	}

	result := make([]shortcut, 0, len(raw))
	for index, pair := range raw {
		compiled, cleaned, err := compileShortcutPattern(pair[0])
		if err != nil {
			return nil, fmt.Errorf("shortcuts[%d]: compile regex %q: %w", index, pair[0], err)
		}
		result = append(result, shortcut{Pattern: compiled, rawPattern: cleaned, URL: pair[1]})
	}
	return result, nil
}

func saveShortcuts(path string, list []shortcut) error {
	raw := make([][2]string, len(list))
	for index, s := range list {
		raw[index] = [2]string{s.rawPattern, s.URL}
	}

	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal shortcuts: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write shortcuts file: %w", err)
	}
	return nil
}

func shortcutsAsJSON(list []shortcut) ([]byte, error) {
	raw := make([][2]string, len(list))
	for index, s := range list {
		raw[index] = [2]string{s.rawPattern, s.URL}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("marshal shortcuts: %w", err)
	}
	return data, nil
}
