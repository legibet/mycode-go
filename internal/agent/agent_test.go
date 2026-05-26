package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/internal/provider"
)

type fakeAdapter struct {
	spec     provider.Spec
	turns    [][]provider.StreamEvent
	requests []provider.Request
}

func (f *fakeAdapter) Spec() provider.Spec {
	return f.spec
}

func (f *fakeAdapter) StreamTurn(_ context.Context, req provider.Request) <-chan provider.StreamEvent {
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

type slowProviderAdapter struct {
	spec provider.Spec
}

func (a *slowProviderAdapter) Spec() provider.Spec {
	return a.spec
}

func (a *slowProviderAdapter) StreamTurn(ctx context.Context, _ provider.Request) <-chan provider.StreamEvent {
	out := make(chan provider.StreamEvent, 1)
	go func() {
		defer close(out)
		out <- provider.StreamEvent{Type: "thinking_delta", Text: "working"}
		<-ctx.Done()
	}()
	return out
}

func TestChatPersistsReasoningBlocks(t *testing.T) {
	dir := t.TempDir()
	agent, err := New(Agent{
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
		Adapter: &fakeAdapter{
			spec: provider.Spec{ID: "openai"},
			turns: [][]provider.StreamEvent{{
				{Type: "thinking_delta", Text: "hidden "},
				{Type: "text_delta", Text: "Visible answer"},
				{Type: "message_done", Msg: new(message.AssistantMessage([]message.Block{
					message.ThinkingBlock("hidden ", nil),
					message.TextBlock("Visible answer", nil),
				}, "openai", "gpt-5.4", "", "", 0, nil))},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	persisted := []message.Message{}
	events := collectEvents(agent.Chat(t.Context(), message.UserTextMessage("hello", nil), func(msg message.Message) error {
		persisted = append(persisted, msg)
		return nil
	}))

	if len(events) != 3 || events[0].Type != "reasoning" || events[1].Type != "reasoning_done" || events[2].Type != "text" {
		t.Fatalf("unexpected events: %#v", events)
	}
	durationMs, ok := events[1].Data["duration_ms"].(int)
	if !ok {
		t.Fatalf("reasoning_done missing duration_ms: %#v", events[1].Data)
	}
	if len(persisted) < 2 || len(persisted[1].Content) != 2 || persisted[1].Content[0].Type != "thinking" {
		t.Fatalf("unexpected persisted messages: %#v", persisted)
	}
	thinkingBlock := persisted[1].Content[0]
	if thinkingBlock.Meta == nil || thinkingBlock.Meta["duration_ms"] != durationMs {
		t.Fatalf("thinking block missing duration_ms in meta: %#v", thinkingBlock)
	}
}

func TestChatPersistsPartialAssistantOnProviderCancel(t *testing.T) {
	dir := t.TempDir()
	adapter := &slowProviderAdapter{spec: provider.Spec{ID: "openai"}}
	agent, err := New(Agent{
		Model:              "gpt-5.5",
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

	ctx, cancel := context.WithCancel(context.Background())
	persisted := []message.Message{}
	stream := agent.Chat(ctx, message.UserTextMessage("hello", nil), func(msg message.Message) error {
		persisted = append(persisted, msg)
		return nil
	})

	var first Event
	select {
	case first = <-stream:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first event")
	}
	if first.Type != "reasoning" || first.Data["delta"] != "working" {
		t.Fatalf("unexpected first event: %#v", first)
	}
	cancel()
	remaining := collectEvents(stream)

	if len(remaining) != 1 || remaining[0].Type != "error" || remaining[0].Data["message"] != "cancelled" {
		t.Fatalf("unexpected remaining events: %#v", remaining)
	}
	if len(persisted) != 2 {
		t.Fatalf("unexpected persisted messages: %#v", persisted)
	}
	last := persisted[1]
	if last.Role != "assistant" || len(last.Content) != 1 {
		t.Fatalf("unexpected partial assistant: %#v", last)
	}
	if last.Content[0].Type != "thinking" || last.Content[0].Text != "working" {
		t.Fatalf("unexpected partial content: %#v", last.Content)
	}
	if _, ok := last.Content[0].Meta["duration_ms"].(int); !ok {
		t.Fatalf("missing thinking duration: %#v", last.Content[0].Meta)
	}
	if last.Meta["provider"] != "openai" || last.Meta["model"] != "gpt-5.5" || last.Meta["context_window"] != 128000 {
		t.Fatalf("unexpected partial meta: %#v", last.Meta)
	}
}

func TestChatRespectsExplicitTurnLimit(t *testing.T) {
	dir := t.TempDir()
	agent, err := New(Agent{
		Model:              "gpt-5.4",
		Provider:           "openai",
		CWD:                dir,
		SessionDir:         filepath.Join(dir, "session"),
		SessionID:          "session",
		MaxTurns:           2,
		MaxTokens:          4096,
		ContextWindow:      128000,
		CompactThreshold:   0.8,
		SupportsImageInput: true,
		SupportsPDFInput:   true,
		Adapter: &fakeAdapter{
			spec: provider.Spec{ID: "openai"},
			turns: [][]provider.StreamEvent{
				{{
					Type: "message_done",
					Msg: new(message.AssistantMessage([]message.Block{
						message.ToolUseBlock("call-1", "read", map[string]any{"path": "missing.txt"}, nil),
					}, "openai", "gpt-5.4", "", "", 0, nil)),
				}},
				{{
					Type: "message_done",
					Msg: new(message.AssistantMessage([]message.Block{
						message.ToolUseBlock("call-2", "read", map[string]any{"path": "missing.txt"}, nil),
					}, "openai", "gpt-5.4", "", "", 0, nil)),
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(t.Context(), message.UserTextMessage("hello", nil), nil))
	last := events[len(events)-1]
	if last.Type != "error" || last.Data["message"] != "max_turns reached" {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestChatPassesSessionIDToProviderRequest(t *testing.T) {
	dir := t.TempDir()
	adapter := &fakeAdapter{
		spec: provider.Spec{ID: "openai"},
		turns: [][]provider.StreamEvent{{
			{
				Type: "message_done",
				Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("ok", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
			},
		}},
	}
	agent, err := New(Agent{
		Model:              "gpt-5.4",
		Provider:           "openai",
		CWD:                dir,
		SessionDir:         filepath.Join(dir, "session-explicit"),
		SessionID:          "session-explicit",
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

	collectEvents(agent.Chat(t.Context(), message.UserTextMessage("hello", nil), nil))

	if len(adapter.requests) != 1 || adapter.requests[0].SessionID != "session-explicit" {
		t.Fatalf("unexpected requests: %#v", adapter.requests)
	}
}

func TestChatDeniesToolOutsidePermissionLevel(t *testing.T) {
	dir := t.TempDir()
	agent, err := New(Agent{
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
		Permission:         config.PermissionConfig{Level: "readonly", Mode: "deny"},
		Adapter: &fakeAdapter{
			spec: provider.Spec{ID: "openai"},
			turns: [][]provider.StreamEvent{
				{{
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
				}},
				{{
					Type: "message_done",
					Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("blocked", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(t.Context(), message.UserTextMessage("hello", nil), nil))

	found := false
	for _, event := range events {
		if event.Type == "tool_done" && event.Data["output"] == permissions.DeniedOutput {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected denied tool_done event: %#v", events)
	}
}

func TestChatStreamingToolCancelKeepsLiveOutput(t *testing.T) {
	dir := t.TempDir()
	agent, err := New(Agent{
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
		Permission:         config.PermissionConfig{Level: "yolo"},
		Adapter: &fakeAdapter{
			spec: provider.Spec{ID: "openai"},
			turns: [][]provider.StreamEvent{{
				{
					Type: "message_done",
					Msg: new(message.AssistantMessage([]message.Block{
						message.ToolUseBlock("call-1", "bash", map[string]any{
							"command": "printf 'live\\n'; sleep 1",
						}, nil),
					}, "openai", "gpt-5.4", "", "", 0, nil)),
				},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := agent.Chat(ctx, message.UserTextMessage("run bash", nil), nil)
	events := []Event{}
	for event := range stream {
		events = append(events, event)
		if event.Type == "tool_output" {
			cancel()
			agent.Cancel()
			break
		}
	}
	events = append(events, collectEvents(stream)...)

	var toolDone *Event
	for i := range events {
		if events[i].Type == "tool_done" {
			toolDone = &events[i]
			break
		}
	}
	if toolDone == nil {
		t.Fatalf("missing tool_done: %#v", events)
	}
	if toolDone.Data["output"] != "live\nerror: cancelled" || toolDone.Data["is_error"] != true {
		t.Fatalf("unexpected tool_done: %#v", toolDone.Data)
	}
}

func TestCompactRequestOmitsReasoningEffort(t *testing.T) {
	dir := t.TempDir()
	adapter := &fakeAdapter{
		spec: provider.Spec{ID: "openai"},
		turns: [][]provider.StreamEvent{
			{{
				Type: "message_done",
				Msg: new(message.AssistantMessage([]message.Block{
					message.TextBlock("answer", nil),
				}, "openai", "gpt-5.4", "", "", 90, nil)),
			}},
			{{
				Type: "message_done",
				Msg: new(message.AssistantMessage([]message.Block{
					message.TextBlock("summary", nil),
				}, "openai", "gpt-5.4", "", "", 0, nil)),
			}},
		},
	}
	agent, err := New(Agent{
		Model:              "gpt-5.4",
		Provider:           "openai",
		CWD:                dir,
		SessionDir:         filepath.Join(dir, "session"),
		SessionID:          "session",
		MaxTokens:          4096,
		ContextWindow:      100,
		CompactThreshold:   0.8,
		ReasoningEffort:    "high",
		SupportsImageInput: true,
		SupportsPDFInput:   true,
		Adapter:            adapter,
	})
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(t.Context(), message.UserTextMessage("hello", nil), nil))

	if len(adapter.requests) != 2 {
		t.Fatalf("unexpected requests: %#v", adapter.requests)
	}
	if adapter.requests[0].ReasoningEffort != "high" {
		t.Fatalf("unexpected main request: %#v", adapter.requests[0])
	}
	if adapter.requests[1].ReasoningEffort != "" || len(adapter.requests[1].Tools) != 0 {
		t.Fatalf("unexpected compact request: %#v", adapter.requests[1])
	}
	last := events[len(events)-1]
	if last.Type != "compact" {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestNewRejectsUnsupportedProviderAdapter(t *testing.T) {
	dir := t.TempDir()
	agent, err := New(Agent{
		Model:     "gpt-5.4",
		Provider:  "missing",
		CWD:       dir,
		SessionID: "session",
	})
	if err == nil {
		t.Fatalf("expected error, got agent %#v", agent)
	}
	if err.Error() != "unsupported provider adapter: missing" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func collectEvents(stream <-chan Event) []Event {
	events := []Event{}
	for event := range stream {
		events = append(events, event)
	}
	return events
}
