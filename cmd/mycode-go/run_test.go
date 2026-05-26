package main

import (
	"testing"

	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/session"
)

func TestResolveSession(t *testing.T) {
	t.Run("new session stays draft", func(t *testing.T) {
		store := session.NewStore(t.TempDir())
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
		store := session.NewStore(t.TempDir())
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
		store := session.NewStore(t.TempDir())
		if _, err := resolveSession(store, t.TempDir(), "missing", false); err == nil {
			t.Fatal("expected missing session error")
		}
	})
}
