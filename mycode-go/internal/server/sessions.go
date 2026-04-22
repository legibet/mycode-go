package server

import (
	"net/http"

	"github.com/legibet/mycode-go/internal/core"
)

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
