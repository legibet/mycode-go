package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"sync"
	"time"

	agentpkg "github.com/legibet/mycode-go/internal/agent"
	"github.com/legibet/mycode-go/internal/message"
)

type runStatus string

const (
	runStatusRunning   runStatus = "running"
	runStatusCompleted runStatus = "completed"
	runStatusFailed    runStatus = "failed"
	runStatusCancelled runStatus = "cancelled"

	finishedRunTTL = 5 * time.Minute
)

type activeRunError struct {
	RunID string
}

func (e activeRunError) Error() string {
	return e.RunID
}

// runSnapshot is the typed view of an active run returned to callers.
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
	cancel     context.CancelFunc
}

func newRunState(id, sessionID string, userMessage message.Message, baseMessages []message.Message, agent *agentpkg.Agent, cancel context.CancelFunc) *runState {
	return &runState{
		id:           id,
		sessionID:    sessionID,
		userMessage:  message.Clone(userMessage),
		baseMessages: message.CloneMessages(baseMessages),
		agent:        agent,
		status:       runStatusRunning,
		nextSeq:      1,
		cancel:       cancel,
	}
}

// infoLocked builds the run summary map. Caller must hold r.mu.
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

func (r *runState) appendEvent(event agentpkg.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	payload := maps.Clone(event.Data)
	r.nextSeq++
	r.events = append(r.events, runEvent{seq: r.nextSeq - 1, typ: event.Type, data: payload})
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

type runManager struct {
	mu              sync.Mutex
	activeBySession map[string]*runState
	runsByID        map[string]*runState
}

func newRunManager() *runManager {
	return &runManager{
		activeBySession: map[string]*runState{},
		runsByID:        map[string]*runState{},
	}
}

func (m *runManager) startRun(sessionID string, userMessage message.Message, baseMessages []message.Message, agent *agentpkg.Agent, onPersist func(message.Message) error) (map[string]any, error) {
	m.pruneFinishedRuns()

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.activeBySession[sessionID]; existing != nil {
		return nil, activeRunError{RunID: existing.id}
	}

	ctx, cancel := context.WithCancel(context.Background())
	state := newRunState(newID(), sessionID, userMessage, baseMessages, agent, cancel)
	m.activeBySession[sessionID] = state
	m.runsByID[state.id] = state

	go m.run(ctx, state, onPersist)
	return state.info(), nil
}

func (m *runManager) getRun(runID string) *runState {
	m.pruneFinishedRuns()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runsByID[runID]
}

func (m *runManager) snapshotSession(sessionID string) *runSnapshot {
	m.mu.Lock()
	state := m.activeBySession[sessionID]
	m.mu.Unlock()
	if state == nil {
		return nil
	}
	snap := state.snapshot()
	return &snap
}

func (m *runManager) cancelRun(runID string) map[string]any {
	state := m.getRun(runID)
	if state == nil {
		return nil
	}
	state.agent.Cancel()
	if state.cancel != nil {
		state.cancel()
	}
	return state.info()
}

func (m *runManager) hasActiveRun(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeBySession[sessionID] != nil
}

func (m *runManager) run(ctx context.Context, state *runState, onPersist func(message.Message) error) {
	var lastError string
	for event := range state.agent.Chat(ctx, state.userMessage, onPersist) {
		if event.Type == "error" {
			if messageText, _ := event.Data["message"].(string); messageText != "" {
				lastError = messageText
			}
		}
		state.appendEvent(event)
	}

	switch lastError {
	case "cancelled":
		state.finish(runStatusCancelled, lastError)
	case "":
		state.finish(runStatusCompleted, "")
	default:
		state.finish(runStatusFailed, lastError)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeBySession[state.sessionID] == state {
		delete(m.activeBySession, state.sessionID)
	}
}

func (m *runManager) pruneFinishedRuns() {
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

func newID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
