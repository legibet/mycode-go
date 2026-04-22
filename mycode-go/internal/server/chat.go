package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/legibet/mycode-go/internal/core"
)

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

	afterValue := strings.TrimSpace(r.URL.Query().Get("after"))
	after := 0
	if afterValue != "" {
		value, err := strconv.Atoi(afterValue)
		if err != nil || value < 0 {
			writeDetailError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = value
	}
	initialPending, initialFinished, err := a.svc.RunEventsAfter(runID, after)
	if err != nil {
		writeCoreError(w, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeDetailError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	headers := w.Header()
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
	headers.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	lastSeq := after
	pending := initialPending
	finished := initialFinished
	for {
		if pending == nil {
			var err error
			pending, finished, err = a.svc.RunEventsAfter(runID, lastSeq)
			if err != nil {
				return
			}
		}
		for _, event := range pending {
			if err := writeSSE(w, event); err != nil {
				return
			}
			lastSeq = eventSeq(event, lastSeq)
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

func (a *app) handleConfig(w http.ResponseWriter, r *http.Request) {
	resp, err := a.svc.Config(r.URL.Query().Get("cwd"))
	if err != nil {
		writeCoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeCoreError(w http.ResponseWriter, err error) {
	if statusErr, ok := err.(*core.StatusError); ok {
		writeDetailError(w, statusErr.Status, statusErr.Detail)
		return
	}
	writeDetailError(w, http.StatusInternalServerError, err.Error())
}
