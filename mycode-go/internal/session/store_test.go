package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legibet/mycode-go/internal/message"
)

func TestNewStoreUsesMycodeHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".mycode")
	t.Setenv("MYCODE_HOME", home)

	store := NewStore("")
	expected := filepath.Join(home, "sessions")
	if store.dataDir != expected {
		t.Fatalf("unexpected data dir: %s", store.dataDir)
	}
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("expected sessions dir: %v", err)
	}
}

func TestCreateSessionDoesNotPrecreateToolOutputDir(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.SessionDir(data.Session.ID), "tool-output")); !os.IsNotExist(err) {
		t.Fatalf("expected no tool-output dir, got err=%v", err)
	}
}

func TestCreateSessionPreservesExistingMessages(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.CreateSession("s1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("s1", message.UserTextMessage("hello", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	if _, err := store.CreateSession("s1", "/tmp"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || len(loaded.Messages) != 1 || loaded.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("unexpected loaded session: %#v", loaded)
	}
}

func TestLoadSessionTreatsInvalidMetaAsMissing(t *testing.T) {
	store := NewStore(t.TempDir())
	sessionDir := store.SessionDir("broken")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession("broken")
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil {
		t.Fatalf("expected missing session, got %#v", loaded)
	}
}

func TestNewSessionUsesV7FormatVersion(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("s1", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if data.Session.MessageFormatVersion != 7 {
		t.Fatalf("expected format version 7, got %d", data.Session.MessageFormatVersion)
	}
}

func TestAppendMessageNormalizesSessionTitle(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.CreateSession("s1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("s1", message.UserTextMessage("first line\nsecond line", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	meta, err := store.readMeta("s1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "first line second line" {
		t.Fatalf("unexpected title: %q", meta.Title)
	}
}

func TestAppendMessageTruncatesSessionTitleByRune(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.CreateSession("s1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	title := "prefix " + strings.Repeat("界", 60)
	if err := store.AppendMessage("s1", message.UserTextMessage(title, nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	meta, err := store.readMeta("s1")
	if err != nil {
		t.Fatal(err)
	}
	expected := "prefix " + strings.Repeat("界", 41)
	if meta.Title != expected {
		t.Fatalf("unexpected title: %q", meta.Title)
	}
}

func TestLoadSessionLeavesOrphanToolUseInPlace(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := data.Session.ID

	if err := store.AppendMessage(sessionID, message.AssistantMessage([]message.Block{
		message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
	}, "anthropic", "gpt-5.4", "", "", 0, nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	metaBefore, err := store.readMeta(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rawBefore, err := os.ReadFile(filepath.Join(store.SessionDir(sessionID), "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Load is read-only: orphan tool_use stays in the visible history; the
	// provider adapter closes it at replay time.
	if loaded == nil || len(loaded.Messages) != 1 {
		t.Fatalf("expected single assistant message, got %#v", loaded)
	}
	if loaded.Messages[0].Role != "assistant" || loaded.Messages[0].Content[0].Type != "tool_use" {
		t.Fatalf("unexpected loaded content: %#v", loaded.Messages[0])
	}

	metaAfter, err := store.readMeta(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	rawAfter, err := os.ReadFile(filepath.Join(store.SessionDir(sessionID), "messages.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if metaAfter.UpdatedAt != metaBefore.UpdatedAt {
		t.Fatalf("LoadSession mutated meta.updated_at: %q -> %q", metaBefore.UpdatedAt, metaAfter.UpdatedAt)
	}
	if string(rawAfter) != string(rawBefore) {
		t.Fatal("LoadSession appended to messages.jsonl; expected read-only behavior")
	}
}

func TestAppendRewindNonexistentSessionIsNoop(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.AppendRewind("missing", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.SessionDir("missing")); !os.IsNotExist(err) {
		t.Fatalf("expected no session dir, got err=%v", err)
	}
}

func TestApplyRewindMultipleMarkers(t *testing.T) {
	messages := []message.Message{
		message.UserTextMessage("a", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("b", nil)}, "p", "m", "", "", 0, nil),
		message.UserTextMessage("c", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("d", nil)}, "p", "m", "", "", 0, nil),
		BuildRewindEvent(2),
		message.UserTextMessage("e", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("f", nil)}, "p", "m", "", "", 0, nil),
		BuildRewindEvent(2),
		message.UserTextMessage("g", nil),
	}

	visible := ApplyRewind(messages)
	if len(visible) != 3 || visible[0].Content[0].Text != "a" || visible[1].Content[0].Text != "b" || visible[2].Content[0].Text != "g" {
		t.Fatalf("unexpected rewind result: %#v", visible)
	}
}

func TestLoadSessionPreservesCompactMarkersInline(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := data.Session.ID

	compactMarker := message.BuildMessage("compact", []message.Block{message.TextBlock("summary text", nil)}, map[string]any{"provider": "p", "model": "m"})
	for _, msg := range []message.Message{
		message.UserTextMessage("hello", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("hi", nil)}, "p", "m", "", "", 0, nil),
		compactMarker,
		message.UserTextMessage("next", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("latest reply", nil)}, "p", "m", "", "", 0, nil),
	} {
		if err := store.AppendMessage(sessionID, msg, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 5 {
		t.Fatalf("expected compact marker preserved inline, got %d messages", len(loaded.Messages))
	}
	if loaded.Messages[2].Role != "compact" || loaded.Messages[2].Content[0].Text != "summary text" {
		t.Fatalf("unexpected compact marker: %#v", loaded.Messages[2])
	}
}

func TestRewindAfterCompactKeepsCompactMarker(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := data.Session.ID

	compactMarker := message.BuildMessage("compact", []message.Block{message.TextBlock("summary of hello+hi", nil)}, map[string]any{"provider": "p", "model": "m"})
	for _, msg := range []message.Message{
		message.UserTextMessage("hello", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("hi", nil)}, "p", "m", "", "", 0, nil),
		compactMarker,
		message.UserTextMessage("explain X", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("X is...", nil)}, "p", "m", "", "", 0, nil),
	} {
		if err := store.AppendMessage(sessionID, msg, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}

	// Rewind back to "explain X" target (visible index 3).
	if err := store.AppendRewind(sessionID, 3); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, message.UserTextMessage("explain Y instead", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Messages) != 4 {
		t.Fatalf("unexpected reloaded messages: %#v", reloaded.Messages)
	}
	if reloaded.Messages[2].Role != "compact" {
		t.Fatalf("expected compact marker at index 2, got %#v", reloaded.Messages[2])
	}
	if reloaded.Messages[3].Content[0].Text != "explain Y instead" {
		t.Fatalf("expected new user message after rewind, got %#v", reloaded.Messages[3])
	}
}

func TestRewindTruncatesPastOpenToolUse(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := data.Session.ID

	for _, msg := range []message.Message{
		message.UserTextMessage("hello", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("hi", nil)}, "p", "m", "", "", 0, nil),
		message.AssistantMessage([]message.Block{
			message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
		}, "p", "m", "", "", 0, nil),
	} {
		if err := store.AppendMessage(sessionID, msg, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.AppendRewind(sessionID, 2); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 2 || loaded.Messages[0].Content[0].Text != "hello" || loaded.Messages[1].Content[0].Text != "hi" {
		t.Fatalf("unexpected messages after rewind: %#v", loaded.Messages)
	}
}
