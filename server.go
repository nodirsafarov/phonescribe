// server.go — HTTP server: serves the UI, lists sheet tabs, starts jobs, and
// streams per-row progress as Server-Sent Events.
//
// Routes:
//   GET  /              → embedded HTML UI
//   GET  /tabs          → JSON list of sheet tab titles
//   POST /run           → starts a job (returns {jobId})
//   GET  /events/{id}   → SSE stream of Events for the given job
//
// SSE catch-up: handleEvents replays the full event history before going live,
// so a browser tab that connects late (or reconnects) still sees everything.

package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"google.golang.org/api/sheets/v4"
)

//go:embed ui.html
var uiFS embed.FS

type Server struct {
	sheets        *sheets.Service
	spreadsheetID string
	picker        Picker
	registry      *JobRegistry
	requestDelay  time.Duration
	tmpl          *template.Template
}

func NewServer(srv *sheets.Service, spreadsheetID string, picker Picker, requestDelay time.Duration) (*Server, error) {
	tmpl, err := template.ParseFS(uiFS, "ui.html")
	if err != nil {
		return nil, fmt.Errorf("parse embedded ui.html: %w", err)
	}
	return &Server{
		sheets:        srv,
		spreadsheetID: spreadsheetID,
		picker:        picker,
		registry:      NewJobRegistry(),
		requestDelay:  requestDelay,
		tmpl:          tmpl,
	}, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/tabs", s.handleTabs)
	mux.HandleFunc("/run", s.handleRun)
	mux.HandleFunc("/events/", s.handleEvents)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := map[string]any{
		"SpreadsheetID": s.spreadsheetID,
		"GeminiOn":      s.picker != nil,
	}
	if err := s.tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleTabs(w http.ResponseWriter, _ *http.Request) {
	tabs, err := SheetTabs(s.sheets, s.spreadsheetID)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tabs": tabs})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req struct {
		Tab     string `json:"tab"`
		FromRow int    `json:"fromRow"`
		ToRow   int    `json:"toRow"`
		DryRun  bool   `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Tab) == "" {
		writeJSONError(w, http.StatusBadRequest, "tab required")
		return
	}
	if req.FromRow < 1 || req.ToRow < req.FromRow {
		writeJSONError(w, http.StatusBadRequest, "invalid row range")
		return
	}
	if req.ToRow-req.FromRow > 999 {
		writeJSONError(w, http.StatusBadRequest, "max 1000 rows per job")
		return
	}

	cfg := JobConfig{
		SpreadsheetID: s.spreadsheetID,
		TabName:       req.Tab,
		FromRow:       req.FromRow,
		ToRow:         req.ToRow,
		DryRun:        req.DryRun,
		RequestDelay:  s.requestDelay,
	}
	job, err := s.registry.Start(cfg, s.sheets, s.picker)
	if err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobId": job.ID})
}

// handleEvents streams a job's events as Server-Sent Events. On every loop it
// takes a snapshot starting from the cursor it has already sent, flushes the
// delta, then blocks on the job's wake-up channel (or a keep-alive timer)
// until more events arrive or the job finishes.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/events/")
	job := s.registry.Get(id)
	if job == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sigCh, unsub := job.Subscribe()
	defer unsub()

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()

	sent := 0
	for {
		events, finished := job.Snapshot(sent)
		for _, e := range events {
			payload, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			sent++
		}
		flusher.Flush()

		if finished {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			flusher.Flush()
			return
		}

		select {
		case <-sigCh:
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
