package agent

import (
	"strings"
	"testing"

	"github.com/legibet/mycode-go/internal/message"
)

func TestShouldCompactRespectsThresholdBoundaries(t *testing.T) {
	if !ShouldCompact(80000, 100000, 0.8) {
		t.Fatal("expected compact at threshold")
	}
	if ShouldCompact(79999, 100000, 0.8) {
		t.Fatal("did not expect compact below threshold")
	}
	if ShouldCompact(99999, 100000, 0) {
		t.Fatal("did not expect compact when disabled")
	}
	if ShouldCompact(0, 100000, 0.8) || ShouldCompact(50000, 0, 0.8) {
		t.Fatal("did not expect compact without usage or context")
	}
}

func TestApplyCompactReplayWithoutMarkerReturnsInput(t *testing.T) {
	messages := []message.Message{
		message.UserTextMessage("hi", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("hello", nil)}, "p", "m", "", "", 0, nil),
	}
	out := ApplyCompactReplay(messages, "")
	if len(out) != 2 {
		t.Fatalf("expected unchanged messages, got %d", len(out))
	}
}

func TestApplyCompactReplayPreservesRoleOrderAfterSummary(t *testing.T) {
	cases := []struct {
		name             string
		tail             []message.Message
		transcriptPath   string
		expectedRoles    []string
		expectedTailText []string
		continueNow      bool
	}{
		{
			name:           "empty tail",
			transcriptPath: "/tmp/messages.jsonl",
			expectedRoles:  []string{"user"},
			continueNow:    true,
		},
		{
			name: "assistant tail",
			tail: []message.Message{
				message.AssistantMessage([]message.Block{message.TextBlock("more", nil)}, "p", "m", "", "", 0, nil),
			},
			expectedRoles:    []string{"user", "assistant"},
			expectedTailText: []string{"more"},
			continueNow:      true,
		},
		{
			name: "user tail",
			tail: []message.Message{
				message.UserTextMessage("next request", nil),
			},
			expectedRoles:    []string{"user", "assistant", "user"},
			expectedTailText: []string{compactAck, "next request"},
			continueNow:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			messages := []message.Message{
				message.UserTextMessage("hello", nil),
				message.AssistantMessage([]message.Block{message.TextBlock("hi", nil)}, "p", "m", "", "", 0, nil),
				BuildCompactEvent("summary text", "p", "m", 0),
			}
			messages = append(messages, tc.tail...)

			projected := ApplyCompactReplay(messages, tc.transcriptPath)
			if len(projected) != len(tc.expectedRoles) {
				t.Fatalf("unexpected projected messages: %#v", projected)
			}
			for i, role := range tc.expectedRoles {
				if projected[i].Role != role {
					t.Fatalf("unexpected role sequence: %#v", projected)
				}
			}
			body := projected[0].Content[0].Text
			if !strings.Contains(body, "summary text") {
				t.Fatalf("expected summary in continuation, got %q", body)
			}
			if (strings.Contains(body, continuationFooter)) != tc.continueNow {
				t.Fatalf("unexpected continuation footer in %q", body)
			}
			if tc.transcriptPath != "" && !strings.Contains(body, tc.transcriptPath) {
				t.Fatalf("expected transcript hint in %q", body)
			}
			for i, text := range tc.expectedTailText {
				if projected[i+1].Content[0].Text != text {
					t.Fatalf("unexpected projected tail: %#v", projected)
				}
			}
		})
	}
}

func TestApplyCompactReplayUsesLatestMarkerAndTranscriptHint(t *testing.T) {
	older := BuildCompactEvent("old", "p", "m", 0)
	newer := BuildCompactEvent("new", "p", "m", 0)
	messages := []message.Message{
		message.UserTextMessage("a", nil),
		older,
		message.UserTextMessage("b", nil),
		newer,
		message.UserTextMessage("c", nil),
	}
	projected := ApplyCompactReplay(messages, "/tmp/messages.jsonl")
	body := projected[0].Content[0].Text
	if !strings.Contains(body, "new") {
		t.Fatalf("expected newer summary in projection, got %q", body)
	}
	if strings.Contains(body, "old") {
		t.Fatalf("did not expect older summary in projection, got %q", body)
	}
	if !strings.Contains(body, "/tmp/messages.jsonl") {
		t.Fatalf("expected transcript hint in projection, got %q", body)
	}
}

func TestBuildCompactEventMetadata(t *testing.T) {
	event := BuildCompactEvent("summary", "anthropic", "claude", 0)
	if event.Role != "compact" {
		t.Fatalf("expected compact role, got %s", event.Role)
	}
	if _, ok := event.Meta["total_tokens"]; ok {
		t.Fatal("did not expect total_tokens when zero")
	}
	if _, ok := event.Meta["compacted_count"]; ok {
		t.Fatal("v7 compact events do not carry compacted_count")
	}

	withUsage := BuildCompactEvent("summary", "anthropic", "claude", 1234)
	if withUsage.Meta["total_tokens"] != 1234 {
		t.Fatalf("expected total_tokens=1234, got %v", withUsage.Meta["total_tokens"])
	}
}
