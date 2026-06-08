package main

import (
	"context"
	"path/filepath"
	"testing"

	agentpkg "github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/provider"
	"github.com/legibet/mycode-go/session"
	"github.com/legibet/mycode-go/tools"
)

func TestResolveSession(t *testing.T) {
	t.Run("new session stays draft", func(t *testing.T) {
		store := newRunTestStore(t)
		resolved, err := resolveSession(store, t.TempDir(), "", false)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.ID == "" || len(resolved.Messages) != 0 {
			t.Fatalf("unexpected session: %#v", resolved)
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

		resolved, err := resolveSession(store, cwd, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if resolved.ID != "second" || len(resolved.Messages) != 1 || resolved.Messages[0].Content[0].Text != "hello" {
			t.Fatalf("unexpected session: %#v", resolved)
		}
	})

	t.Run("missing explicit session", func(t *testing.T) {
		store := newRunTestStore(t)
		if _, err := resolveSession(store, t.TempDir(), "missing", false); err == nil {
			t.Fatal("expected missing session error")
		}
	})
}

func TestRunNoninteractiveFailsOnPermissionDenied(t *testing.T) {
	dir := t.TempDir()
	agent, err := agentpkg.New(agentpkg.Config{
		Model:              "gpt-5.4",
		Provider:           "openai",
		CWD:                dir,
		SessionDir:         filepath.Join(dir, "session"),
		SessionID:          "session",
		MaxTokens:          4096,
		ContextWindow:      128000,
		CompactThreshold:   0.8,
		SupportsImageInput: true,
		SupportsPDFInput:   true,
		Hooks: tools.Hooks{
			BeforeTool: []tools.BeforeToolHook{
				permissions.ToolHook(config.PermissionConfig{Level: "readonly", Mode: "deny"}, nil, dir, dir, nil),
			},
		},
		Adapter: &runFakeAdapter{
			turns: [][]provider.StreamEvent{
				{
					{
						Type: "message_done",
						Msg: new(message.AssistantMessage([]message.Block{
							message.ToolUseBlock("call-1", "edit", map[string]any{
								"path": "file.txt",
								"edits": []map[string]any{{
									"oldText": "a",
									"newText": "b",
								}},
							}, nil),
						}, "openai", "gpt-5.4", "", "", 0, nil)),
					},
				},
				{
					{
						Type: "message_done",
						Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("blocked", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	reply, errorMessage := runNoninteractive(context.Background(), agent, message.UserTextMessage("go", nil), nil)
	if reply != "blocked" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if errorMessage != permissions.DeniedOutput {
		t.Fatalf("unexpected error: %q", errorMessage)
	}
}

func TestRunNoninteractiveKeepsAtReferencesInPrompt(t *testing.T) {
	dir := t.TempDir()
	adapter := &runFakeAdapter{
		turns: [][]provider.StreamEvent{{
			{
				Type: "message_done",
				Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("ok", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
			},
		}},
	}
	agent, err := agentpkg.New(agentpkg.Config{
		Model:              "gpt-5.4",
		Provider:           "openai",
		CWD:                dir,
		SessionDir:         filepath.Join(dir, "session"),
		SessionID:          "session",
		MaxTokens:          4096,
		ContextWindow:      128000,
		CompactThreshold:   0.8,
		SupportsImageInput: true,
		SupportsPDFInput:   true,
		Adapter:            adapter,
	})
	if err != nil {
		t.Fatal(err)
	}

	prompt := "explain @decorator and @scope/pkg"
	reply, errorMessage := runNoninteractive(context.Background(), agent, message.UserTextMessage(prompt, nil), nil)
	if reply != "ok" || errorMessage != "" {
		t.Fatalf("unexpected run result: reply=%q error=%q", reply, errorMessage)
	}
	if len(adapter.requests) != 1 || len(adapter.requests[0].Messages) != 1 {
		t.Fatalf("unexpected provider requests: %#v", adapter.requests)
	}
	got := adapter.requests[0].Messages[0].Content[0].Text
	if got != prompt {
		t.Fatalf("unexpected prompt: %q", got)
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

type runFakeAdapter struct {
	turns    [][]provider.StreamEvent
	requests []provider.Request
}

func (f *runFakeAdapter) Spec() provider.Spec {
	return provider.Spec{ID: "openai"}
}

func (f *runFakeAdapter) StreamTurn(_ context.Context, req provider.Request) <-chan provider.StreamEvent {
	out := make(chan provider.StreamEvent, 8)
	f.requests = append(f.requests, req)
	events := []provider.StreamEvent{}
	if len(f.turns) > 0 {
		events = f.turns[0]
		f.turns = f.turns[1:]
	}
	go func() {
		defer close(out)
		for _, event := range events {
			out <- event
		}
	}()
	return out
}
