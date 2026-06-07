package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/session"
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

func TestBuildRunUserMessageWithAttachments(t *testing.T) {
	cwd := t.TempDir()
	codePath := filepath.Join(cwd, "main.py")
	imagePath := filepath.Join(cwd, "diagram.png")
	imageData := mustDecodeBase64(t, "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j1X8AAAAASUVORK5CYII=")
	if err := os.WriteFile(codePath, []byte("print('hello')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, imageData, 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := "check @" + codePath + " @" + imagePath
	msg := buildRunUserMessage(prompt, cwd)
	resolvedCodePath, err := filepath.EvalSymlinks(codePath)
	if err != nil {
		t.Fatal(err)
	}

	if msg.Role != "user" || len(msg.Content) != 3 {
		t.Fatalf("unexpected message: %#v", msg)
	}
	if msg.Content[0].Type != "text" || msg.Content[0].Text != prompt {
		t.Fatalf("unexpected prompt block: %#v", msg.Content[0])
	}
	if msg.Content[1].Type != "text" || msg.Content[1].Meta["path"] != resolvedCodePath || !strings.Contains(msg.Content[1].Text, "print('hello')") {
		t.Fatalf("unexpected text attachment: %#v", msg.Content[1])
	}
	if msg.Content[2].Type != "image" || msg.Content[2].Data != base64.StdEncoding.EncodeToString(imageData) || msg.Content[2].Name != "diagram.png" {
		t.Fatalf("unexpected image attachment: %#v", msg.Content[2])
	}
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
