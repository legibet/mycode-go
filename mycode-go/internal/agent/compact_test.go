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

func TestApplyCompactReplayContinueNowOnEmptyTail(t *testing.T) {
	compactEvent := BuildCompactEvent("summary text", "p", "m", 0)
	messages := []message.Message{
		message.UserTextMessage("hello", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("hi", nil)}, "p", "m", "", "", 0, nil),
		compactEvent,
	}
	projected := ApplyCompactReplay(messages, "/tmp/messages.jsonl")
	if len(projected) != 1 {
		t.Fatalf("expected single user continuation, got %d", len(projected))
	}
	if projected[0].Role != "user" {
		t.Fatalf("expected user role, got %s", projected[0].Role)
	}
	body := projected[0].Content[0].Text
	if !strings.Contains(body, "summary text") {
		t.Fatalf("expected summary in continuation, got %q", body)
	}
	if !strings.Contains(body, continuationFooter) {
		t.Fatal("expected continuation footer when tail is empty")
	}
	if !strings.Contains(body, "/tmp/messages.jsonl") {
		t.Fatal("expected transcript hint when path is provided")
	}
}

func TestApplyCompactReplayUserLedTailGetsAck(t *testing.T) {
	compactEvent := BuildCompactEvent("summary", "p", "m", 0)
	messages := []message.Message{
		message.UserTextMessage("hi", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("there", nil)}, "p", "m", "", "", 0, nil),
		compactEvent,
		message.UserTextMessage("next request", nil),
	}
	projected := ApplyCompactReplay(messages, "")
	if len(projected) != 3 {
		t.Fatalf("expected user/assistant/user, got %d", len(projected))
	}
	if projected[0].Role != "user" || projected[1].Role != "assistant" || projected[2].Role != "user" {
		t.Fatalf("unexpected role sequence: %v %v %v", projected[0].Role, projected[1].Role, projected[2].Role)
	}
	if projected[1].Content[0].Text != compactAck {
		t.Fatalf("expected ack message, got %q", projected[1].Content[0].Text)
	}
	if strings.Contains(projected[0].Content[0].Text, continuationFooter) {
		t.Fatal("did not expect continuation footer for user-led tail")
	}
}

func TestApplyCompactReplayAssistantLedTailContinuesNow(t *testing.T) {
	compactEvent := BuildCompactEvent("summary", "p", "m", 0)
	messages := []message.Message{
		message.UserTextMessage("hi", nil),
		message.AssistantMessage([]message.Block{message.TextBlock("there", nil)}, "p", "m", "", "", 0, nil),
		compactEvent,
		message.AssistantMessage([]message.Block{message.TextBlock("more", nil)}, "p", "m", "", "", 0, nil),
	}
	projected := ApplyCompactReplay(messages, "")
	if len(projected) != 2 {
		t.Fatalf("expected user/assistant, got %d", len(projected))
	}
	if !strings.Contains(projected[0].Content[0].Text, continuationFooter) {
		t.Fatal("expected continuation footer for assistant-led tail")
	}
}

func TestApplyCompactReplayUsesLatestMarker(t *testing.T) {
	older := BuildCompactEvent("old", "p", "m", 0)
	newer := BuildCompactEvent("new", "p", "m", 0)
	messages := []message.Message{
		message.UserTextMessage("a", nil),
		older,
		message.UserTextMessage("b", nil),
		newer,
		message.UserTextMessage("c", nil),
	}
	projected := ApplyCompactReplay(messages, "")
	body := projected[0].Content[0].Text
	if !strings.Contains(body, "new") {
		t.Fatalf("expected newer summary in projection, got %q", body)
	}
	if strings.Contains(body, "old") {
		t.Fatalf("did not expect older summary in projection, got %q", body)
	}
}

func TestBuildCompactEventOmitsZeroTotalTokens(t *testing.T) {
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
}

func TestBuildCompactEventIncludesTotalTokens(t *testing.T) {
	event := BuildCompactEvent("summary", "anthropic", "claude", 1234)
	if event.Meta["total_tokens"] != 1234 {
		t.Fatalf("expected total_tokens=1234, got %v", event.Meta["total_tokens"])
	}
}
