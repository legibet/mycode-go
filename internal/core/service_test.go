package core

import (
	"errors"
	"net/http"
	"testing"

	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/provider"
	"github.com/legibet/mycode-go/internal/session"
)

func TestServiceStartChatRejectsActiveSessionBeforeRewind(t *testing.T) {
	t.Setenv("MYCODE_HOME", t.TempDir())

	cwd := t.TempDir()
	store := session.NewStore(t.TempDir())
	manager := NewRunManager(nil)
	service := NewService(Options{Store: store, Runs: manager})
	sessionID := "session-1"

	if _, err := store.CreateSession(sessionID, cwd); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []message.Message{
		message.UserTextMessage("first", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("ok", nil)}, "openai", "gpt-5.4", "", "", 0, nil),
		message.UserTextMessage("second", nil),
	} {
		if err := store.AppendMessage(sessionID, msg, cwd); err != nil {
			t.Fatal(err)
		}
	}

	adapter := &blockingAdapter{
		spec:    provider.Spec{ID: "openai"},
		release: make(chan struct{}),
	}
	agent := newTestAgent(t, adapter)
	run, err := manager.startRun(sessionID, message.UserTextMessage("active", nil), nil, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.cancelRun(run["id"].(string))
	waitRunEvent(t, manager, run["id"].(string), "text")

	_, err = service.StartChat(ChatRequest{
		SessionID: sessionID,
		Message:   "new prompt",
		CWD:       cwd,
		Provider:  "openai",
		Model:     "gpt-5.4",
		APIKey:    "sk-test",
		RewindTo:  new(0),
	})
	expectStatus(t, err, http.StatusConflict)

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || len(loaded.Messages) != 3 {
		t.Fatalf("unexpected session after rejected rewind: %#v", loaded)
	}
	if loaded.Messages[0].Content[0].Text != "first" || loaded.Messages[2].Content[0].Text != "second" {
		t.Fatalf("session was unexpectedly rewound: %#v", loaded.Messages)
	}
}

func TestServiceClearAndDeleteRejectActiveSession(t *testing.T) {
	cwd := t.TempDir()
	store := session.NewStore(t.TempDir())
	manager := NewRunManager(nil)
	service := NewService(Options{Store: store, Runs: manager})
	sessionID := "session-1"

	if _, err := store.CreateSession(sessionID, cwd); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, message.UserTextMessage("keep me", nil), cwd); err != nil {
		t.Fatal(err)
	}

	adapter := &blockingAdapter{
		spec:    provider.Spec{ID: "openai"},
		release: make(chan struct{}),
	}
	agent := newTestAgent(t, adapter)
	run, err := manager.startRun(sessionID, message.UserTextMessage("active", nil), nil, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.cancelRun(run["id"].(string))
	waitRunEvent(t, manager, run["id"].(string), "text")

	expectStatus(t, service.ClearSession(sessionID), http.StatusConflict)
	expectStatus(t, service.DeleteSession(sessionID), http.StatusConflict)

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || len(loaded.Messages) != 1 || loaded.Messages[0].Content[0].Text != "keep me" {
		t.Fatalf("unexpected session after rejected clear/delete: %#v", loaded)
	}
}

func expectStatus(t *testing.T, err error, status int) {
	t.Helper()
	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected status %d, got err=%v", status, err)
	}
	if statusErr.Status != status {
		t.Fatalf("expected status %d, got %d (%#v)", status, statusErr.Status, statusErr.Detail)
	}
}
