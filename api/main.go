package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed all:web/dist
var staticFiles embed.FS

type server struct {
	store     *Store
	dataDir   string
	audioDir  string
	aiURL     string
	aiClient  *http.Client
	apiLogs   *lineLogBuffer
	startedAt time.Time
	// azureSTTURL overrides the Azure Speech endpoint in tests; empty uses
	// the standard regional host.
	azureSTTURL     string
	ttsMu           sync.Mutex
	audioCleanupMu  sync.Mutex
	ttsInit         sync.Once
	ttsCond         *sync.Cond
	ttsQueue        []*ttsJob
	ttsJobs         map[string]*ttsJob
	ttsActive       *ttsJob
	ttsClearing     bool
	ttsSequence     uint64
	ttsEnqueued     uint64
	ttsCompleted    uint64
	ttsFailed       uint64
	ttsCancelled    uint64
	ttsPromoted     uint64
	ttsLastDuration time.Duration
	ttsLastError    string
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		resp, err := http.Get("http://127.0.0.1:8080/api/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		resp.Body.Close()
		return
	}

	apiLogs := newLineLogBuffer(500)
	log.SetOutput(io.MultiWriter(os.Stdout, apiLogs))
	dataDir := envOr("APP_DATA_DIR", "./data")
	audioDir := envOr("AUDIO_DIR", filepath.Join(dataDir, "audio"))
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(audioDir, 0o750); err != nil {
		log.Fatal(err)
	}
	if copied, bytes, err := migrateLegacyAudio(filepath.Join(dataDir, "audio"), audioDir); err != nil {
		log.Printf("speech storage migration failed: %v", err)
	} else if copied > 0 {
		log.Printf("speech storage migration copied=%d audio_bytes=%d", copied, bytes)
	}
	dbPath := filepath.Join(dataDir, "vocab.db")
	dbURL := sqliteDSN(dbPath)
	store, err := openStore(dbURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	s := &server{store: store, dataDir: dataDir, audioDir: audioDir, aiURL: strings.TrimRight(os.Getenv("AI_SERVICE_URL"), "/"), aiClient: &http.Client{Timeout: 6 * time.Minute}, apiLogs: apiLogs, startedAt: time.Now()}
	s.startTTSQueue()
	addr := envOr("APP_ADDR", ":8080")
	log.Printf("Vocab Self-host listening on %s", addr)
	if err := http.ListenAndServe(addr, s); err != nil {
		log.Fatal(err)
	}
}

func sqliteDSN(dbPath string) string {
	dsnPath := filepath.ToSlash(dbPath)
	if runtime.GOOS == "windows" && len(dsnPath) >= 2 && dsnPath[1] == ':' {
		dsnPath = "/" + dsnPath
	}
	dbURL := (&url.URL{Scheme: "file", Path: dsnPath}).String()
	return dbURL + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/healthz" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.handleAPI(w, r)
		return
	}
	s.handleStatic(w, r)
}

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	assets, err := fs.Sub(staticFiles, "web/dist")
	if err != nil {
		http.Error(w, "UI assets are unavailable", http.StatusInternalServerError)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	if _, err = fs.Stat(assets, name); err != nil {
		name = "index.html"
	}
	file, err := assets.Open(name)
	if err != nil {
		http.Error(w, "UI assets are unavailable; build the web app", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	if seeker, ok := file.(io.ReadSeeker); ok {
		if strings.HasSuffix(name, ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		http.ServeContent(w, r, name, time.Time{}, seeker)
		return
	}
	http.NotFound(w, r)
}

func (s *server) handleAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch parts[0] {
	case "cards":
		s.handleCards(w, r, parts[1:])
	case "dictionary":
		s.handleDictionary(w, r, parts[1:])
	case "export":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		bundle, err := s.store.export(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="vocab-backup.json"`)
		writeJSON(w, http.StatusOK, bundle)
	case "import", "sync":
		s.handleImportOrSync(w, r, parts[0])
	case "tts":
		s.handleTTS(w, r)
	case "audio":
		if len(parts) == 1 {
			s.handleAudioManagement(w, r)
		} else {
			s.handleAudio(w, r, parts[1:])
		}
	case "ops":
		s.handleOps(w, r, parts[1:])
	case "pronunciation":
		if len(parts) == 2 && parts[1] == "model" {
			s.handlePronunciationModel(w, r)
		} else if len(parts) == 1 {
			s.handlePronunciation(w, r)
		} else {
			writeError(w, http.StatusNotFound, "not found")
		}
	case "gloss":
		s.proxyAI(w, r, "/v1/gloss")
	case "settings":
		s.handleSettings(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *server) handleCards(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			cards, err := s.store.listCards(r.Context(), r.URL.Query().Get("scope"), r.URL.Query().Get("q"))
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, cards)
		case http.MethodPost:
			var input CardInput
			if !decodeJSON(w, r, &input, 32<<10) {
				return
			}
			if err := validateCardInput(input); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			id := strings.TrimSpace(input.ID)
			if id == "" {
				id = randomID()
			}
			card, err := s.store.createCard(r.Context(), input, id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			s.enqueueCardSpeech(card)
			writeJSON(w, http.StatusCreated, card)
		default:
			methodNotAllowed(w)
		}
		return
	}
	id := parts[0]
	if len(parts) == 1 && parts[0] == "batch" {
		s.handleCardsBatch(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "review" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var req ReviewRequest
		if !decodeJSON(w, r, &req, 4<<10) {
			return
		}
		card, duplicate, err := s.store.reviewCard(r.Context(), id, req)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "card not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"card": card, "duplicate": duplicate})
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		card, err := s.store.getCard(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "card not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, card)
	case http.MethodPut:
		var input CardInput
		if !decodeJSON(w, r, &input, 32<<10) {
			return
		}
		if err := validateCardInput(input); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		card, err := s.store.updateCard(r.Context(), id, input)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "card not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.enqueueCardSpeech(card)
		writeJSON(w, http.StatusOK, card)
	case http.MethodDelete:
		err := s.store.deleteCard(r.Context(), id)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "card not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (s *server) handleDictionary(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) == 1 && parts[0] == "import" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		var payload struct {
			Entries []DictionaryEntry `json:"entries"`
		}
		if !decodeJSON(w, r, &payload, 64<<20) {
			return
		}
		count, err := s.store.importDictionary(r.Context(), payload.Entries)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"imported": count})
		return
	}
	if len(parts) != 0 || r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []DictionaryEntry{})
		return
	}
	entries, err := s.store.searchDictionary(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *server) handleImportOrSync(w http.ResponseWriter, r *http.Request, kind string) {
	if kind == "sync" && r.Method == http.MethodGet {
		bundle, err := s.store.export(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		bundle.Dictionary = nil
		writeJSON(w, http.StatusOK, bundle)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var bundle BackupBundle
	if !decodeJSON(w, r, &bundle, 64<<20) {
		return
	}
	if err := s.store.importBundle(r.Context(), bundle); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, card := range bundle.Cards {
		s.enqueueCardSpeech(card)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "synced", "cards": len(bundle.Cards)})
}

func (s *server) handleTTS(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req ttsRequest
	if !decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if err := validateTTSRequest(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filename := speechFilename(req.Voice, req.Text)
	fullPath := filepath.Join(s.audioStorageDir(), filename)
	wasStored := false
	if _, err := os.Stat(fullPath); err == nil {
		wasStored = true
	}
	if err := s.enqueueSpeech(r.Context(), req.Text, req.Voice, ttsPriorityInteractive, true); err != nil {
		log.Printf("TTS request priority=interactive duration=%.3fs result=error", time.Since(started).Seconds())
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		log.Printf("TTS request priority=interactive duration=%.3fs result=missing", time.Since(started).Seconds())
		writeError(w, http.StatusServiceUnavailable, "generated audio is not available")
		return
	}
	log.Printf("TTS request priority=interactive duration=%.3fs audio_bytes=%d previously_stored=%t", time.Since(started).Seconds(), info.Size(), wasStored)
	writeJSON(w, http.StatusOK, map[string]any{"audioUrl": "/api/audio/" + filename, "stored": true})
}

func (s *server) handleAudio(w http.ResponseWriter, r *http.Request, parts []string) {
	if r.Method != http.MethodGet || len(parts) != 1 {
		methodNotAllowed(w)
		return
	}
	name := path.Base(parts[0])
	if !strings.HasSuffix(name, ".wav") || len(name) != 68 {
		http.NotFound(w, r)
		return
	}
	started := time.Now()
	fullPath := filepath.Join(s.audioStorageDir(), name)
	info, err := os.Stat(fullPath)
	if err != nil {
		log.Printf("TTS audio delivery result=not_found duration=%.3fs", time.Since(started).Seconds())
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeFile(w, r, fullPath)
	log.Printf("Speech audio delivery duration=%.3fs audio_bytes=%d", time.Since(started).Seconds(), info.Size())
}

func (s *server) proxyAI(w http.ResponseWriter, r *http.Request, endpoint string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request is too large")
		return
	}
	if endpoint == "/v1/gloss" {
		if body, err = s.injectTextAI(r.Context(), body); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	response, status, err := s.callAIBytes(r.Context(), endpoint, body)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(response)
}

func (s *server) callAI(ctx context.Context, endpoint string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	return s.callAIBytes(ctx, endpoint, body)
}

func (s *server) callAIBytes(ctx context.Context, endpoint string, body []byte) ([]byte, int, error) {
	return s.callAIRequest(ctx, http.MethodPost, endpoint, body)
}

func (s *server) callAIRequest(ctx context.Context, method, endpoint string, body []byte) ([]byte, int, error) {
	if s.aiURL == "" {
		return nil, http.StatusServiceUnavailable, errors.New("local AI service is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, method, s.aiURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.aiClient.Do(req)
	if err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("AI service unavailable: %w", err)
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var detail struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(response, &detail)
		if detail.Error == "" {
			detail.Error = "AI service request failed"
		}
		return nil, resp.StatusCode, errors.New(detail.Error)
	}
	return response, resp.StatusCode, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "request must contain one JSON value")
		return false
	}
	return true
}

func validateCardInput(c CardInput) error {
	word := strings.TrimSpace(c.Word)
	if word == "" || len(word) > 100 {
		return errors.New("word must contain 1-100 characters")
	}
	if len(c.Definition) > 5000 || len(c.ContextText) > 10000 || len(c.ContextMeaning) > 5000 || len(c.Source) > 500 {
		return errors.New("one or more fields are too long")
	}
	return nil
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(b[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}
