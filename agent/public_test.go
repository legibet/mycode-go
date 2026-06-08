package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legibet/mycode-go/agent"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/provider"
	"github.com/legibet/mycode-go/tools"
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

func TestPublicAgentUsesTempToolOutputDirWithoutSession(t *testing.T) {
	cwd := t.TempDir()
	adapter := &turnAdapter{
		turns: [][]provider.StreamEvent{
			{{
				Type: "message_done",
				Msg: new(message.AssistantMessage([]message.Block{
					message.ToolUseBlock("call-large", "bash", map[string]any{
						"command": "head -c 5000001 /dev/zero | tr '\\0' x",
					}, nil),
				}, "fake", "fake-model", "", "", 0, nil)),
			}},
			{{
				Type: "message_done",
				Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("done", nil)}, "fake", "fake-model", "", "", 0, nil)),
			}},
		},
	}
	runtime, err := agent.New(agent.Config{
		Provider:  "fake",
		Model:     "fake-model",
		CWD:       cwd,
		SessionID: "memory-session",
		Adapter:   adapter,
		ToolSpecs: tools.DefaultSpecs(),
	})
	if err != nil {
		t.Fatalf("agent.New returned error: %v", err)
	}

	events := collectEvents(runtime.Chat(t.Context(), message.UserTextMessage("run", nil), agent.ChatOptions{}))

	output := ""
	for _, event := range events {
		if event.Type == "tool_done" {
			output, _ = event.Data["output"].(string)
			break
		}
	}
	if output == "" {
		t.Fatalf("missing tool_done output: %#v", events)
	}
	logPath := fullOutputPath(output)
	if logPath == "" {
		t.Fatalf("missing spill path in output: %q", output)
	}
	if strings.HasPrefix(logPath, cwd+string(os.PathSeparator)) {
		t.Fatalf("spill path should not be under cwd: %s", logPath)
	}
	wantPrefix := filepath.Join(os.TempDir(), "mycode", "memory-session", "tool-output") + string(os.PathSeparator)
	if !strings.HasPrefix(logPath, wantPrefix) {
		t.Fatalf("spill path = %s, want prefix %s", logPath, wantPrefix)
	}
	if _, err := os.Stat(filepath.Join(cwd, "tool-output")); !os.IsNotExist(err) {
		t.Fatalf("cwd tool-output should not exist, stat error: %v", err)
	}
}

func TestPublicAgentCompactDefaultAndDisableBehavior(t *testing.T) {
	for _, tc := range []struct {
		name         string
		disable      bool
		wantCompact  bool
		wantReqCount int
	}{
		{name: "default", wantCompact: true, wantReqCount: 2},
		{name: "disabled", disable: true, wantCompact: false, wantReqCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &turnAdapter{
				turns: [][]provider.StreamEvent{
					{{
						Type: "message_done",
						Msg: new(message.AssistantMessage([]message.Block{
							message.TextBlock("answer", nil),
						}, "fake", "fake-model", "", "", 80, nil)),
					}},
					{{
						Type: "message_done",
						Msg: new(message.AssistantMessage([]message.Block{
							message.TextBlock("summary", nil),
						}, "fake", "fake-model", "", "", 0, nil)),
					}},
				},
			}
			runtime, err := agent.New(agent.Config{
				Provider:       "fake",
				Model:          "fake-model",
				CWD:            t.TempDir(),
				Adapter:        adapter,
				ContextWindow:  100,
				DisableCompact: tc.disable,
			})
			if err != nil {
				t.Fatalf("agent.New returned error: %v", err)
			}

			events := collectEvents(runtime.Chat(t.Context(), message.UserTextMessage("hello", nil), agent.ChatOptions{}))

			if len(adapter.requests) != tc.wantReqCount {
				t.Fatalf("request count = %d, want %d", len(adapter.requests), tc.wantReqCount)
			}
			if got := hasEvent(events, "compact"); got != tc.wantCompact {
				t.Fatalf("compact event = %v, want %v; events=%#v", got, tc.wantCompact, events)
			}
		})
	}
}

type turnAdapter struct {
	turns    [][]provider.StreamEvent
	requests []provider.Request
}

func (a *turnAdapter) Spec() provider.Spec {
	return provider.Spec{ID: "fake"}
}

func (a *turnAdapter) StreamTurn(_ context.Context, req provider.Request) <-chan provider.StreamEvent {
	events := make(chan provider.StreamEvent, 8)
	a.requests = append(a.requests, req)
	turn := []provider.StreamEvent{}
	if len(a.turns) > 0 {
		turn = a.turns[0]
		a.turns = a.turns[1:]
	}
	go func() {
		defer close(events)
		for _, event := range turn {
			events <- event
		}
	}()
	return events
}

func fullOutputPath(output string) string {
	const marker = "Full output: "
	start := strings.Index(output, marker)
	if start < 0 {
		return ""
	}
	start += len(marker)
	end := strings.Index(output[start:], "]")
	if end < 0 {
		return ""
	}
	return output[start : start+end]
}

func hasEvent(events []agent.Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func collectEvents(events <-chan agent.Event) []agent.Event {
	var out []agent.Event
	for event := range events {
		out = append(out, event)
	}
	return out
}
