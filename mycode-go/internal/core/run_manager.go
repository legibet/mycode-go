// RunManager owns one in-flight chat per session and brokers permission
// prompts so /api/runs/{id}/stream can replay events to clients that
// reconnect mid-run.

package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	agentpkg "github.com/legibet/mycode-go/internal/agent"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/permissions"
)

type runStatus string

const (
	runStatusRunning   runStatus = "running"
	runStatusCompleted runStatus = "completed"
	runStatusFailed    runStatus = "failed"
	runStatusCancelled runStatus = "cancelled"

	// finishedRunTTL keeps completed runs addressable so a client reconnecting
	// right after completion can still pull the final events.
	finishedRunTTL = 5 * time.Minute
)

// EventPayload is what desktop adapters consume.
type EventPayload struct {
	RunID     string         `json:"run_id"`
	SessionID string         `json:"session_id"`
	Event     map[string]any `json:"event"`
}

type EventSink func(EventPayload)

// ActiveRunError signals the session already has a run in flight.
type ActiveRunError struct {
	RunID string
}

func (e ActiveRunError) Error() string {
	return e.RunID
}

type runSnapshot struct {
	Run           map[string]any
	Messages      []message.Message
	PendingEvents []map[string]any
}

type runEvent struct {
	seq  int
	typ  string
	data map[string]any
}

func (e runEvent) payload() map[string]any {
	payload := maps.Clone(e.data)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["seq"] = e.seq
	payload["type"] = e.typ
	return payload
}

type runState struct {
	id           string
	sessionID    string
	userMessage  message.Message
	baseMessages []message.Message
	agent        *agentpkg.Agent

	mu         sync.RWMutex
	status     runStatus
	errorText  string
	events     []runEvent
	nextSeq    int
	finishedAt time.Time
	cancelReq  bool
	cancel     context.CancelFunc
	done       chan struct{}

	pendingDecisions map[string]chan permissions.ReviewDecision
}

func newRunState(id, sessionID string, userMessage message.Message, baseMessages []message.Message, agent *agentpkg.Agent, cancel context.CancelFunc) *runState {
	return &runState{
		id:               id,
		sessionID:        sessionID,
		userMessage:      message.Clone(userMessage),
		baseMessages:     message.CloneMessages(baseMessages),
		agent:            agent,
		status:           runStatusRunning,
		nextSeq:          1,
		cancel:           cancel,
		done:             make(chan struct{}),
		pendingDecisions: map[string]chan permissions.ReviewDecision{},
	}
}

// infoLocked: caller must already hold r.mu.
func (r *runState) infoLocked() map[string]any {
	out := map[string]any{
		"id":         r.id,
		"session_id": r.sessionID,
		"status":     string(r.status),
		"last_seq":   r.nextSeq - 1,
	}
	if r.errorText != "" {
		out["error"] = r.errorText
	}
	return out
}

func (r *runState) info() map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.infoLocked()
}

func (r *runState) appendEvent(event agentpkg.Event) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	payload := maps.Clone(event.Data)
	stored := runEvent{seq: r.nextSeq, typ: event.Type, data: payload}
	r.nextSeq++
	r.events = append(r.events, stored)
	return stored.payload()
}

func (r *runState) addDecision(requestID string, ch chan permissions.ReviewDecision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pendingDecisions[requestID] = ch
}

func (r *runState) removeDecision(requestID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pendingDecisions, requestID)
}

// resolveDecision is non-blocking; returns false if no reviewer is waiting.
func (r *runState) resolveDecision(requestID string, decision permissions.ReviewDecision) bool {
	r.mu.RLock()
	ch := r.pendingDecisions[requestID]
	r.mu.RUnlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- decision:
		return true
	default:
		return false
	}
}

// denyPendingDecisions unblocks every waiting reviewer on cancel so the
// agent goroutine can exit.
func (r *runState) denyPendingDecisions() {
	r.mu.RLock()
	channels := slices.Collect(maps.Values(r.pendingDecisions))
	r.mu.RUnlock()
	for _, ch := range channels {
		select {
		case ch <- permissions.ReviewDeny:
		default:
		}
	}
}

func (r *runState) requestCancel() {
	r.mu.Lock()
	r.cancelReq = true
	r.mu.Unlock()

	if r.cancel != nil {
		r.cancel()
	}
	r.agent.Cancel()
}

func (r *runState) finish(status runStatus, errText string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
	r.errorText = errText
	r.finishedAt = time.Now()
}

func (r *runState) pendingAfter(after int) ([]map[string]any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var pending []map[string]any
	for _, event := range r.events {
		if event.seq > after {
			pending = append(pending, event.payload())
		}
	}
	return pending, r.status != runStatusRunning
}

func (r *runState) snapshot() runSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	messages := append(message.CloneMessages(r.baseMessages), message.Clone(r.userMessage))
	events := make([]map[string]any, 0, len(r.events))
	for _, event := range r.events {
		events = append(events, event.payload())
	}

	return runSnapshot{
		Run:           r.infoLocked(),
		Messages:      messages,
		PendingEvents: events,
	}
}

type RunManager struct {
	mu              sync.Mutex
	activeBySession map[string]*runState
	runsByID        map[string]*runState
	sink            EventSink
}

func NewRunManager(sink EventSink) *RunManager {
	return &RunManager{
		activeBySession: map[string]*runState{},
		runsByID:        map[string]*runState{},
		sink:            sink,
	}
}

// startRun returns ActiveRunError if the session already has a run in flight.
func (m *RunManager) startRun(sessionID string, userMessage message.Message, baseMessages []message.Message, agent *agentpkg.Agent, onPersist func(message.Message) error) (map[string]any, error) {
	m.pruneFinishedRuns()

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.activeBySession[sessionID]; existing != nil {
		return nil, ActiveRunError{RunID: existing.id}
	}

	ctx, cancel := context.WithCancel(context.Background())
	state := newRunState(randomHex16(), sessionID, userMessage, baseMessages, agent, cancel)
	m.activeBySession[sessionID] = state
	m.runsByID[state.id] = state

	go m.run(ctx, state, onPersist)
	return state.info(), nil
}

func (m *RunManager) getRun(runID string) *runState {
	m.pruneFinishedRuns()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runsByID[runID]
}

func (m *RunManager) snapshotSession(sessionID string) *runSnapshot {
	m.mu.Lock()
	state := m.activeBySession[sessionID]
	m.mu.Unlock()
	if state == nil {
		return nil
	}
	snap := state.snapshot()
	return &snap
}

func (m *RunManager) cancelRun(runID string) map[string]any {
	state := m.getRun(runID)
	if state == nil {
		return nil
	}
	state.requestCancel()
	state.denyPendingDecisions()
	<-state.done
	return state.info()
}

// requestDecision blocks the agent goroutine until the UI POSTs to
// /runs/{id}/decide (or ctx is cancelled).
func (m *RunManager) requestDecision(ctx context.Context, sessionID string, request permissions.ReviewRequest) permissions.ReviewDecision {
	m.mu.Lock()
	state := m.activeBySession[sessionID]
	m.mu.Unlock()
	if state == nil {
		return permissions.ReviewDeny
	}

	requestID := randomHex16()
	ch := make(chan permissions.ReviewDecision, 1)
	state.addDecision(requestID, ch)

	m.emit(state, state.appendEvent(agentpkg.Event{
		Type: "permission_request",
		Data: map[string]any{
			"request_id":  requestID,
			"tool_use_id": request.ToolCallID,
			"tool_name":   request.ToolName,
			"preview":     request.Preview,
		},
	}))

	decision := permissions.ReviewDeny
	select {
	case decision = <-ch:
	case <-ctx.Done():
	}

	state.removeDecision(requestID)
	m.emit(state, state.appendEvent(agentpkg.Event{
		Type: "permission_resolved",
		Data: map[string]any{
			"request_id": requestID,
			"decision":   string(decision),
		},
	}))
	return decision
}

func (m *RunManager) resolveDecision(runID, requestID string, decision permissions.ReviewDecision) bool {
	state := m.getRun(runID)
	if state == nil {
		return false
	}
	resolved := state.resolveDecision(requestID, decision)
	if resolved && decision == permissions.ReviewDeny {
		state.requestCancel()
	}
	return resolved
}

func (m *RunManager) hasActiveRun(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeBySession[sessionID] != nil
}

// pendingAfter returns (events, finished, found); the third bool
// distinguishes "run not found" from "no new events yet".
func (m *RunManager) pendingAfter(runID string, after int) ([]map[string]any, bool, bool) {
	state := m.getRun(runID)
	if state == nil {
		return nil, false, false
	}
	pending, finished := state.pendingAfter(after)
	return pending, finished, true
}

func (m *RunManager) emit(state *runState, event map[string]any) {
	if m.sink == nil {
		return
	}
	m.sink(EventPayload{
		RunID:     state.id,
		SessionID: state.sessionID,
		Event:     event,
	})
}

func (m *RunManager) run(ctx context.Context, state *runState, onPersist func(message.Message) error) {
	defer close(state.done)

	var lastError string
	for event := range state.agent.Chat(ctx, state.userMessage, onPersist) {
		if event.Type == "error" {
			if messageText, _ := event.Data["message"].(string); messageText != "" {
				lastError = messageText
			}
		}
		m.emit(state, state.appendEvent(event))
	}

	state.mu.RLock()
	cancelRequested := state.cancelReq
	state.mu.RUnlock()

	status := runStatusCompleted
	switch {
	case cancelRequested, lastError == "cancelled":
		status = runStatusCancelled
	case lastError != "":
		status = runStatusFailed
	}
	state.finish(status, lastError)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeBySession[state.sessionID] == state {
		delete(m.activeBySession, state.sessionID)
	}
}

func (m *RunManager) pruneFinishedRuns() {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for runID, state := range m.runsByID {
		state.mu.RLock()
		finishedAt := state.finishedAt
		state.mu.RUnlock()
		if !finishedAt.IsZero() && now.Sub(finishedAt) >= finishedRunTTL {
			delete(m.runsByID, runID)
		}
	}
}

func randomHex16() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
