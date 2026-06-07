package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/legibet/mycode-go/message"
)

func TestNewStoreUsesExplicitDataDir(t *testing.T) {
	expected := filepath.Join(t.TempDir(), "sessions")

	store := NewStore(expected)
	if store.dataDir != expected {
		t.Fatalf("unexpected data dir: %s", store.dataDir)
	}
	if _, err := os.Stat(expected); err != nil {
		t.Fatalf("expected sessions dir: %v", err)
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

func TestListSessionsFiltersSortsAndLatest(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.CreateSession("first", "/workspace"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := store.CreateSession("second", "/workspace"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, err := store.CreateSession("other", "/other"); err != nil {
		t.Fatal(err)
	}

	sessions, err := store.ListSessions("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != "second" || sessions[1].ID != "first" {
		t.Fatalf("unexpected sessions: %#v", sessions)
	}

	latest, err := store.LatestSession("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.ID != "second" {
		t.Fatalf("unexpected latest session: %#v", latest)
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

	loaded, err := store.LoadSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Session.Title != "first line second line" {
		t.Fatalf("unexpected title: %q", loaded.Session.Title)
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

	loaded, err := store.LoadSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	expected := "prefix " + strings.Repeat("界", 41)
	if loaded.Session.Title != expected {
		t.Fatalf("unexpected title: %q", loaded.Session.Title)
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

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	loadedAgain, err := store.LoadSession(sessionID)
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
	if loadedAgain == nil || len(loadedAgain.Messages) != 1 || loadedAgain.Messages[0].Content[0].Type != "tool_use" {
		t.Fatalf("unexpected second load: %#v", loadedAgain)
	}
}

func TestClearSessionKeepsSessionAddressable(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.CreateSession("s1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("s1", message.UserTextMessage("hello", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	if err := store.ClearSession("s1"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || len(loaded.Messages) != 0 || loaded.Session.Title != DefaultSessionTitle {
		t.Fatalf("unexpected cleared session: %#v", loaded)
	}
	sessions, err := store.ListSessions("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Title != DefaultSessionTitle {
		t.Fatalf("unexpected session list after clear: %#v", sessions)
	}
}

func TestDeleteSessionRemovesSession(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.CreateSession("s1", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage("s1", message.UserTextMessage("hello", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteSession("s1"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil {
		t.Fatalf("unexpected deleted session: %#v", loaded)
	}
	sessions, err := store.ListSessions("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("unexpected session list after delete: %#v", sessions)
	}
}

func TestListSessionsRecoversUnavailableIndex(t *testing.T) {
	for _, name := range []string{"missing", "damaged"} {
		t.Run(name, func(t *testing.T) {
			store := NewStore(t.TempDir())
			if _, err := store.CreateSession("s1", "/tmp"); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendMessage("s1", message.UserTextMessage("Hello", nil), "/tmp"); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "missing":
				if err := os.Remove(store.indexPath()); err != nil {
					t.Fatal(err)
				}
			case "damaged":
				if err := os.WriteFile(store.indexPath(), []byte("{bad json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			sessions, err := store.ListSessions("/tmp")
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != 1 || sessions[0].ID != "s1" || sessions[0].Title != "Hello" {
				t.Fatalf("unexpected recovered sessions: %#v", sessions)
			}
		})
	}
}

func TestLoadSessionAppliesMultipleRewindMarkers(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := data.Session.ID

	initial := []message.Message{
		message.UserTextMessage("a", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("b", nil)}, "p", "m", "", "", 0, nil),
		message.UserTextMessage("c", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("d", nil)}, "p", "m", "", "", 0, nil),
	}
	for _, msg := range initial {
		if err := store.AppendMessage(sessionID, msg, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendRewind(sessionID, 2); err != nil {
		t.Fatal(err)
	}
	for _, msg := range []message.Message{
		message.UserTextMessage("e", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("f", nil)}, "p", "m", "", "", 0, nil),
	} {
		if err := store.AppendMessage(sessionID, msg, "/tmp"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AppendRewind(sessionID, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, message.UserTextMessage("g", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	visible := loaded.Messages
	if len(visible) != 3 || visible[0].Content[0].Text != "a" || visible[1].Content[0].Text != "b" || visible[2].Content[0].Text != "g" {
		t.Fatalf("unexpected rewind result: %#v", visible)
	}
}

func TestLoadSessionAppliesRewindToZero(t *testing.T) {
	store := NewStore(t.TempDir())
	data, err := store.CreateSession("", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := data.Session.ID

	if err := store.AppendMessage(sessionID, message.UserTextMessage("a", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRewind(sessionID, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(sessionID, message.UserTextMessage("fresh", nil), "/tmp"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	visible := loaded.Messages
	if len(visible) != 1 || visible[0].Content[0].Text != "fresh" {
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
