package main

import (
	"context"
	"iter"
	"slices"
	"testing"

	agentpkg "github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/session"
)

func TestResolveSessionID(t *testing.T) {
	t.Run("new session stays draft", func(t *testing.T) {
		store := newRunTestStore(t)
		resolved, err := resolveSessionID(store, t.TempDir(), "", false)
		if err != nil {
			t.Fatal(err)
		}
		if resolved == "" {
			t.Fatal("expected a generated session id")
		}
		sessions, err := store.ListSessions("")
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 0 {
			t.Fatalf("draft session should not be persisted: %#v", sessions)
		}
	})

	t.Run("continue latest", func(t *testing.T) {
		cwd := t.TempDir()
		store := newRunTestStore(t)
		if _, err := store.CreateSession("first", cwd); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateSession("second", cwd); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendMessage("second", message.UserTextMessage("hello", nil), cwd); err != nil {
			t.Fatal(err)
		}

		resolved, err := resolveSessionID(store, cwd, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if resolved != "second" {
			t.Fatalf("unexpected session: %q", resolved)
		}
	})

	t.Run("missing explicit session", func(t *testing.T) {
		store := newRunTestStore(t)
		if _, err := resolveSessionID(store, t.TempDir(), "missing", false); err == nil {
			t.Fatal("expected missing session error")
		}
	})
}

func TestRunNoninteractiveFailsOnPermissionDenied(t *testing.T) {
	agent := &fakeChatAgent{events: []agentpkg.Event{
		{Type: "tool_start", Data: map[string]any{"tool_call": map[string]any{"id": "call-1", "name": "edit"}}},
		{Type: "tool_done", Data: map[string]any{"tool_use_id": "call-1", "output": permissions.DeniedOutput, "is_error": true}},
		{Type: "text", Data: map[string]any{"delta": "blocked"}},
	}}

	reply, errorMessage := runNoninteractive(context.Background(), agent, message.UserTextMessage("go", nil))
	if reply != "blocked" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if errorMessage != permissions.DeniedOutput {
		t.Fatalf("unexpected error: %q", errorMessage)
	}
}

func TestRunNoninteractiveCollectsReplyAndPassesPrompt(t *testing.T) {
	agent := &fakeChatAgent{events: []agentpkg.Event{
		{Type: "text", Data: map[string]any{"delta": "ok"}},
	}}

	prompt := "explain @decorator and @scope/pkg"
	reply, errorMessage := runNoninteractive(context.Background(), agent, message.UserTextMessage(prompt, nil))
	if reply != "ok" || errorMessage != "" {
		t.Fatalf("unexpected run result: reply=%q error=%q", reply, errorMessage)
	}
	if agent.received.Content[0].Text != prompt {
		t.Fatalf("unexpected prompt: %q", agent.received.Content[0].Text)
	}
}

func newRunTestStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type fakeChatAgent struct {
	received message.Message
	events   []agentpkg.Event
}

func (f *fakeChatAgent) ChatMessage(_ context.Context, msg message.Message) iter.Seq[agentpkg.Event] {
	f.received = msg
	return slices.Values(f.events)
}
