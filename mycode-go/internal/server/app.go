package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/legibet/mycode-go/internal/core"
	"github.com/legibet/mycode-go/internal/session"
)

type app struct {
	svc      *core.Service
	serveWeb bool
	webRoot  string
	webFS    fs.FS
	api      *http.ServeMux
}

// NewHandler builds the HTTP handler for the API and optional web UI.
func NewHandler(serveWeb bool) http.Handler {
	return newApp(serveWeb, "", nil, nil)
}

// NewDevHandler builds the API-only handler used with the Vite dev server.
func NewDevHandler() http.Handler {
	return withDevCORS(NewHandler(false))
}

func newApp(serveWeb bool, webRoot string, store *session.Store, runs *core.RunManager) *app {
	resolvedWebRoot := webRoot
	var webFS fs.FS
	if serveWeb {
		resolvedWebRoot = defaultWebRoot(resolvedWebRoot)
		if resolvedWebRoot != "" {
			webFS = os.DirFS(resolvedWebRoot)
		} else {
			webFS = embeddedWebFS()
		}
	}

	mux := http.NewServeMux()
	app := &app{
		svc:      core.NewService(core.Options{Store: store, Runs: runs}),
		serveWeb: serveWeb,
		webRoot:  resolvedWebRoot,
		webFS:    webFS,
		api:      mux,
	}

	mux.HandleFunc("POST /api/chat", app.handleChat)
	mux.HandleFunc("GET /api/runs/{run_id}/stream", app.handleRunStream)
	mux.HandleFunc("POST /api/runs/{run_id}/cancel", app.handleCancelRun)
	mux.HandleFunc("POST /api/runs/{run_id}/decide", app.handleDecideRun)
	mux.HandleFunc("GET /api/config", app.handleConfig)
	mux.HandleFunc("GET /api/settings", app.handleSettings)
	mux.HandleFunc("PUT /api/settings", app.handleUpdateSettings)

	mux.HandleFunc("POST /api/sessions", app.handleCreateSession)
	mux.HandleFunc("GET /api/sessions", app.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{session_id}", app.handleLoadSession)
	mux.HandleFunc("DELETE /api/sessions/{session_id}", app.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{session_id}/clear", app.handleClearSession)

	mux.HandleFunc("GET /api/workspaces/roots", app.handleWorkspaceRoots)
	mux.HandleFunc("GET /api/workspaces/browse", app.handleWorkspaceBrowse)
	mux.HandleFunc("GET /api/workspaces/cwd", app.handleWorkspaceCWD)

	return app
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		a.api.ServeHTTP(w, r)
		return
	}
	if !a.serveWeb {
		http.NotFound(w, r)
		return
	}
	a.serveStatic(w, r)
}

// Chat / runs

func (a *app) handleChat(w http.ResponseWriter, r *http.Request) {
	var req core.ChatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDetailError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := a.svc.StartChat(req)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleRunStream(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")

	after := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			writeDetailError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = value
	}
	batch, err := a.svc.RunEventsAfter(runID, after)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	pending := batch.Events
	finished := batch.Finished

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeDetailError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	headers := w.Header()
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastSeq := after
	for {
		if pending == nil {
			batch, err = a.svc.RunEventsAfter(runID, lastSeq)
			if err != nil {
				return
			}
			pending = batch.Events
			finished = batch.Finished
		}
		for _, event := range pending {
			if err := writeSSE(w, event.Payload()); err != nil {
				return
			}
			lastSeq = event.Seq
			flusher.Flush()
		}

		if finished {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			pending = nil
		}
	}
}

func (a *app) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	resp, err := a.svc.CancelRun(r.PathValue("run_id"))
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleDecideRun(w http.ResponseWriter, r *http.Request) {
	var req core.DecideRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDetailError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := a.svc.DecideRun(r.PathValue("run_id"), req)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Config / settings

func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	resp, err := a.svc.Config(r.URL.Query().Get("cwd"))
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleSettings(w http.ResponseWriter, r *http.Request) {
	resp, err := a.svc.Settings()
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req core.SettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDetailError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := a.svc.UpdateSettings(req)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Sessions

func (a *app) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req core.SessionCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeDetailError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := a.svc.CreateSession(req)
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleListSessions(w http.ResponseWriter, r *http.Request) {
	resp, err := a.svc.ListSessions(r.URL.Query().Get("cwd"))
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleLoadSession(w http.ResponseWriter, r *http.Request) {
	resp, err := a.svc.LoadSession(r.PathValue("session_id"))
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.DeleteSession(r.PathValue("session_id")); err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *app) handleClearSession(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.ClearSession(r.PathValue("session_id")); err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// Workspaces

func (a *app) handleWorkspaceRoots(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.WorkspaceRoots())
}

func (a *app) handleWorkspaceBrowse(w http.ResponseWriter, r *http.Request) {
	resp, err := a.svc.WorkspaceBrowse(r.URL.Query().Get("root"), r.URL.Query().Get("path"))
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *app) handleWorkspaceCWD(w http.ResponseWriter, _ *http.Request) {
	cwd, err := os.Getwd()
	if err != nil {
		writeDetailError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, statErr := os.Stat(cwd)
	writeJSON(w, http.StatusOK, map[string]any{
		"cwd":    cwd,
		"exists": statErr == nil,
	})
}

// Static file serving (SPA fallback to index.html)

func (a *app) serveStatic(w http.ResponseWriter, r *http.Request) {
	if a.webFS == nil {
		http.Error(w, "web UI assets are not available; run with --dev, set MYCODE_WEB_DIST, or build with -tags embedweb", http.StatusServiceUnavailable)
		return
	}
	requested := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if requested == "." || requested == "/" {
		requested = "index.html"
	}
	if a.serveStaticFile(w, r, requested) {
		return
	}
	// SPA fallback so client-side routes resolve to index.html.
	if a.serveStaticFile(w, r, "index.html") {
		return
	}
	http.NotFound(w, r)
}

func (a *app) serveStaticFile(w http.ResponseWriter, r *http.Request, name string) bool {
	info, err := fs.Stat(a.webFS, name)
	if err != nil || info.IsDir() {
		return false
	}
	http.ServeFileFS(w, r, a.webFS, name)
	return true
}

// defaultWebRoot resolves only explicit static asset directories. Empty means
// callers should use the embedded FS.
func defaultWebRoot(webRoot string) string {
	if resolved := resolveWebRoot(webRoot); resolved != "" {
		return resolved
	}
	if raw := strings.TrimSpace(os.Getenv("MYCODE_WEB_DIST")); raw != "" {
		if resolved := resolveWebRoot(raw); resolved != "" {
			return resolved
		}
	}
	return ""
}

func resolveWebRoot(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	resolved := raw
	if absolute, err := filepath.Abs(raw); err == nil {
		resolved = absolute
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}

// HTTP utilities

func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

func writeDetailError(w http.ResponseWriter, status int, detail any) {
	writeJSON(w, status, map[string]any{"detail": detail})
}

// writeCoreError maps a core.StatusError to its HTTP status; falls back to 500.
func writeCoreError(w http.ResponseWriter, err error) {
	if statusErr, ok := errors.AsType[*core.StatusError](err); ok {
		writeDetailError(w, statusErr.Status, statusErr.Detail)
		return
	}
	writeDetailError(w, http.StatusInternalServerError, err.Error())
}

func writeSSE(w io.Writer, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func withDevCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if setDevCORSHeaders(w, r) && r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setDevCORSHeaders(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "http://localhost:5173" && origin != "http://127.0.0.1:5173" {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	return true
}
