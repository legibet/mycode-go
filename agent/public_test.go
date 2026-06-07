package agent_test

import (
	"context"
	"testing"

	"github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/provider"
)

type captureAdapter struct {
	requests []provider.Request
}

func (a *captureAdapter) Spec() provider.Spec {
	return provider.Spec{ID: "fake"}
}

func (a *captureAdapter) StreamTurn(_ context.Context, req provider.Request) <-chan provider.StreamEvent {
	a.requests = append(a.requests, req)
	events := make(chan provider.StreamEvent, 2)
	events <- provider.StreamEvent{Type: "text_delta", Text: "ok"}
	events <- provider.StreamEvent{Type: "message_done", Msg: new(message.BuildMessage("assistant", []message.Block{
		message.TextBlock("ok", nil),
	}, nil))}
	close(events)
	return events
}

func TestPublicAgentRuntimeRunsWithoutAppDefaults(t *testing.T) {
	adapter := &captureAdapter{}
	runtime, err := agent.New(agent.Config{
		Provider: "fake",
		Model:    "fake-model",
		CWD:      t.TempDir(),
		Adapter:  adapter,
	})
	if err != nil {
		t.Fatalf("agent.New returned error: %v", err)
	}

	events := collectEvents(runtime.Chat(t.Context(), message.UserTextMessage("hello", nil), agent.ChatOptions{}))

	if len(events) != 1 || events[0].Type != "text" || events[0].Data["delta"] != "ok" {
		t.Fatalf("unexpected events: %#v", events)
	}
	if len(adapter.requests) != 1 {
		t.Fatalf("captured %d provider requests, want 1", len(adapter.requests))
	}
	if adapter.requests[0].System != "" {
		t.Fatalf("system prompt should be empty by default")
	}
	if len(adapter.requests[0].Tools) != 0 {
		t.Fatalf("default tools = %d, want 0", len(adapter.requests[0].Tools))
	}
}

func collectEvents(events <-chan agent.Event) []agent.Event {
	var out []agent.Event
	for event := range events {
		out = append(out, event)
	}
	return out
}
