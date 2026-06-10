package agent

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/legibet/mycode-go/attachment"
	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/provider"
	"github.com/legibet/mycode-go/session"
	"github.com/legibet/mycode-go/tools"
)

type fakeAdapter struct {
	spec     provider.Spec
	turns    [][]provider.StreamEvent
	requests []provider.Request
}

func (f *fakeAdapter) Spec() provider.Spec { return f.spec }

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

func (a *slowProviderAdapter) Spec() provider.Spec { return a.spec }

func (a *slowProviderAdapter) StreamTurn(ctx context.Context, _ provider.Request) <-chan provider.StreamEvent {
	out := make(chan provider.StreamEvent, 1)
	go func() {
		defer close(out)
		out <- provider.StreamEvent{Type: "thinking_delta", Text: "working"}
		<-ctx.Done()
	}()
	return out
}

func openAIAdapter(turns ...[]provider.StreamEvent) *fakeAdapter {
	return &fakeAdapter{spec: provider.Spec{ID: "openai"}, turns: turns}
}

func newTestStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testMetadata(contextWindow int) *provider.ModelMetadata {
	return &provider.ModelMetadata{
		MaxOutputTokens:    4096,
		ContextWindow:      contextWindow,
		SupportsImageInput: true,
		SupportsPDFInput:   true,
	}
}

func collectEvents(stream iter.Seq[Event]) []Event {
	events := []Event{}
	for event := range stream {
		events = append(events, event)
	}
	return events
}

func hasEvent(events []Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
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

func TestChatPersistsReasoningBlocks(t *testing.T) {
	store := newTestStore(t)
	agent, err := newAgent(Config{
		Model:            "gpt-5.4",
		Provider:         "openai",
		CWD:              t.TempDir(),
		Store:            store,
		SessionID:        "session",
		Metadata:         testMetadata(128000),
		CompactThreshold: 0.8,
	}, openAIAdapter([]provider.StreamEvent{
		{Type: "thinking_delta", Text: "hidden "},
		{Type: "text_delta", Text: "Visible answer"},
		{Type: "message_done", Msg: new(message.AssistantMessage([]message.Block{
			message.ThinkingBlock("hidden ", nil),
			message.TextBlock("Visible answer", nil),
		}, "openai", "gpt-5.4", "", "", 0, nil))},
	}))
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(t.Context(), "hello"))

	if len(events) != 3 || events[0].Type != "reasoning" || events[1].Type != "reasoning_done" || events[2].Type != "text" {
		t.Fatalf("unexpected events: %#v", events)
	}
	durationMs, ok := events[1].Data["duration_ms"].(int)
	if !ok {
		t.Fatalf("reasoning_done missing duration_ms: %#v", events[1].Data)
	}

	data, err := store.LoadSession("session")
	if err != nil || data == nil {
		t.Fatalf("load session: %v", err)
	}
	if len(data.Messages) < 2 || len(data.Messages[1].Content) != 2 || data.Messages[1].Content[0].Type != "thinking" {
		t.Fatalf("unexpected persisted messages: %#v", data.Messages)
	}
	thinking := data.Messages[1].Content[0]
	if thinking.Meta == nil || asInt(thinking.Meta["duration_ms"]) != durationMs {
		t.Fatalf("thinking block missing duration_ms in meta: %#v", thinking)
	}
}

func TestChatPersistsPartialAssistantOnProviderCancel(t *testing.T) {
	store := newTestStore(t)
	agent, err := newAgent(Config{
		Model:            "gpt-5.5",
		Provider:         "openai",
		CWD:              t.TempDir(),
		Store:            store,
		SessionID:        "session",
		Metadata:         testMetadata(128000),
		CompactThreshold: 0.8,
	}, &slowProviderAdapter{spec: provider.Spec{ID: "openai"}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	next, stop := iter.Pull(agent.Chat(ctx, "hello"))
	defer stop()

	first, ok := next()
	if !ok {
		t.Fatal("missing first event")
	}
	if first.Type != "reasoning" || first.Data["delta"] != "working" {
		t.Fatalf("unexpected first event: %#v", first)
	}
	cancel()
	remaining := []Event{}
	for {
		event, ok := next()
		if !ok {
			break
		}
		remaining = append(remaining, event)
	}

	if len(remaining) != 1 || remaining[0].Type != "error" || remaining[0].Data["message"] != "cancelled" {
		t.Fatalf("unexpected remaining events: %#v", remaining)
	}

	data, err := store.LoadSession("session")
	if err != nil || data == nil {
		t.Fatalf("load session: %v", err)
	}
	if len(data.Messages) != 2 {
		t.Fatalf("unexpected persisted messages: %#v", data.Messages)
	}
	last := data.Messages[1]
	if last.Role != "assistant" || len(last.Content) != 1 {
		t.Fatalf("unexpected partial assistant: %#v", last)
	}
	if last.Content[0].Type != "thinking" || last.Content[0].Text != "working" {
		t.Fatalf("unexpected partial content: %#v", last.Content)
	}
	if last.Content[0].Meta["duration_ms"] == nil {
		t.Fatalf("missing thinking duration: %#v", last.Content[0].Meta)
	}
	if last.Meta["provider"] != "openai" || last.Meta["model"] != "gpt-5.5" || asInt(last.Meta["context_window"]) != 128000 {
		t.Fatalf("unexpected partial meta: %#v", last.Meta)
	}
}

func TestChatRespectsExplicitTurnLimit(t *testing.T) {
	agent, err := newAgent(Config{
		Model:            "gpt-5.4",
		Provider:         "openai",
		CWD:              t.TempDir(),
		MaxTurns:         2,
		Metadata:         testMetadata(128000),
		CompactThreshold: 0.8,
		Tools:            []tools.Spec{tools.Read},
	}, openAIAdapter(
		[]provider.StreamEvent{{
			Type: "message_done",
			Msg: new(message.AssistantMessage([]message.Block{
				message.ToolUseBlock("call-1", "read", map[string]any{"path": "missing.txt"}, nil),
			}, "openai", "gpt-5.4", "", "", 0, nil)),
		}},
		[]provider.StreamEvent{{
			Type: "message_done",
			Msg: new(message.AssistantMessage([]message.Block{
				message.ToolUseBlock("call-2", "read", map[string]any{"path": "missing.txt"}, nil),
			}, "openai", "gpt-5.4", "", "", 0, nil)),
		}},
	))
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(t.Context(), "hello"))
	last := events[len(events)-1]
	if last.Type != "error" || last.Data["message"] != "max_turns reached" {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestChatPassesSessionIDToProviderRequest(t *testing.T) {
	adapter := openAIAdapter([]provider.StreamEvent{{
		Type: "message_done",
		Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("ok", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
	}})
	agent, err := newAgent(Config{
		Model:            "gpt-5.4",
		Provider:         "openai",
		CWD:              t.TempDir(),
		SessionID:        "session-explicit",
		Metadata:         testMetadata(128000),
		CompactThreshold: 0.8,
	}, adapter)
	if err != nil {
		t.Fatal(err)
	}

	collectEvents(agent.Chat(t.Context(), "hello"))

	if len(adapter.requests) != 1 || adapter.requests[0].SessionID != "session-explicit" {
		t.Fatalf("unexpected requests: %#v", adapter.requests)
	}
}

func TestChatDeniesToolOutsidePermissionLevel(t *testing.T) {
	dir := t.TempDir()
	agent, err := newAgent(Config{
		Model:            "gpt-5.4",
		Provider:         "openai",
		CWD:              dir,
		Metadata:         testMetadata(128000),
		CompactThreshold: 0.8,
		Tools:            []tools.Spec{tools.Read, tools.Write, tools.Edit, tools.Bash},
		Hooks: tools.Hooks{
			BeforeTool: []tools.BeforeToolHook{
				permissions.ToolHook(config.PermissionConfig{Level: "readonly", Mode: "deny"}, nil, dir, dir, nil),
			},
		},
	}, openAIAdapter(
		[]provider.StreamEvent{{
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
		[]provider.StreamEvent{{
			Type: "message_done",
			Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("blocked", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
		}},
	))
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(t.Context(), "hello"))

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
	agent, err := newAgent(Config{
		Model:            "gpt-5.4",
		Provider:         "openai",
		CWD:              t.TempDir(),
		Metadata:         testMetadata(128000),
		CompactThreshold: 0.8,
		Tools:            []tools.Spec{tools.Read, tools.Write, tools.Edit, tools.Bash},
	}, openAIAdapter([]provider.StreamEvent{{
		Type: "message_done",
		Msg: new(message.AssistantMessage([]message.Block{
			message.ToolUseBlock("call-1", "bash", map[string]any{
				"command": "printf 'live\\n'; sleep 1",
			}, nil),
		}, "openai", "gpt-5.4", "", "", 0, nil)),
	}}))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := []Event{}
	for event := range agent.Chat(ctx, "run bash") {
		events = append(events, event)
		if event.Type == "tool_output" {
			cancel()
			agent.Cancel()
		}
	}

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

func TestChatCancelBetweenToolsKeepsCompletedResults(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	agent, err := newAgent(Config{
		Model:            "gpt-5.4",
		Provider:         "openai",
		CWD:              t.TempDir(),
		Store:            store,
		SessionID:        "session",
		Metadata:         testMetadata(128000),
		CompactThreshold: 0.8,
		Tools: []tools.Spec{
			{
				Name: "first",
				Runner: func(context.Context, tools.ToolCall) tools.Result {
					cancel()
					return tools.Result{Output: "first result"}
				},
			},
			{
				Name: "second",
				Runner: func(context.Context, tools.ToolCall) tools.Result {
					return tools.Result{Output: "second result"}
				},
			},
		},
	}, openAIAdapter([]provider.StreamEvent{{
		Type: "message_done",
		Msg: new(message.AssistantMessage([]message.Block{
			message.ToolUseBlock("call-1", "first", nil, nil),
			message.ToolUseBlock("call-2", "second", nil, nil),
		}, "openai", "gpt-5.4", "", "", 0, nil)),
	}}))
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(ctx, "hello"))

	var done []Event
	for _, event := range events {
		if event.Type == "tool_done" {
			done = append(done, event)
		}
	}
	if len(done) != 2 {
		t.Fatalf("unexpected events: %#v", events)
	}
	if done[0].Data["tool_use_id"] != "call-1" || done[0].Data["output"] != "first result" || done[0].Data["is_error"] != false {
		t.Fatalf("unexpected first tool result: %#v", done[0])
	}
	if done[1].Data["tool_use_id"] != "call-2" || done[1].Data["output"] != "error: cancelled" || done[1].Data["is_error"] != true {
		t.Fatalf("unexpected second tool result: %#v", done[1])
	}

	data, err := store.LoadSession("session")
	if err != nil || data == nil {
		t.Fatalf("load session: %v", err)
	}
	last := data.Messages[len(data.Messages)-1]
	if last.Role != "user" || len(last.Content) != 2 {
		t.Fatalf("unexpected persisted tool message: %#v", data.Messages)
	}
	if last.Content[0].ToolUseID != "call-1" || last.Content[0].Output != "first result" {
		t.Fatalf("missing completed tool result: %#v", last.Content)
	}
	if last.Content[1].ToolUseID != "call-2" || last.Content[1].Output != "error: cancelled" || last.Content[1].IsError == nil || !*last.Content[1].IsError {
		t.Fatalf("missing cancelled tool result: %#v", last.Content)
	}
}

func TestCompactRequestOmitsReasoningEffort(t *testing.T) {
	adapter := openAIAdapter(
		[]provider.StreamEvent{{
			Type: "message_done",
			Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("answer", nil)}, "openai", "gpt-5.4", "", "", 90, nil)),
		}},
		[]provider.StreamEvent{{
			Type: "message_done",
			Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("summary", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
		}},
	)
	agent, err := newAgent(Config{
		Model:            "gpt-5.4",
		Provider:         "openai",
		CWD:              t.TempDir(),
		Metadata:         testMetadata(100),
		CompactThreshold: 0.8,
		ReasoningEffort:  "high",
	}, adapter)
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(agent.Chat(t.Context(), "hello"))

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

func TestNewRejectsUnsupportedProvider(t *testing.T) {
	agent, err := New(Config{
		Model:    "gpt-5.4",
		Provider: "missing",
		CWD:      t.TempDir(),
	})
	if err == nil {
		t.Fatalf("expected error, got agent %#v", agent)
	}
	if err.Error() != "unsupported provider adapter: missing" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRuntimeRunsWithoutAppDefaults(t *testing.T) {
	adapter := openAIAdapter([]provider.StreamEvent{
		{Type: "text_delta", Text: "ok"},
		{Type: "message_done", Msg: new(message.BuildMessage("assistant", []message.Block{message.TextBlock("ok", nil)}, nil))},
	})
	runtime, err := newAgent(Config{Provider: "openai", Model: "gpt-5.4", CWD: t.TempDir()}, adapter)
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(runtime.Chat(t.Context(), "hello"))

	if len(events) != 1 || events[0].Type != "text" || events[0].Data["delta"] != "ok" {
		t.Fatalf("unexpected events: %#v", events)
	}
	if adapter.requests[0].System != "" {
		t.Fatalf("system prompt should be empty by default")
	}
	if len(adapter.requests[0].Tools) != 0 {
		t.Fatalf("default tools = %d, want 0", len(adapter.requests[0].Tools))
	}
}

func TestToolOutputDirDefaultsToTempWithoutStore(t *testing.T) {
	cwd := t.TempDir()
	adapter := openAIAdapter(
		[]provider.StreamEvent{{
			Type: "message_done",
			Msg: new(message.AssistantMessage([]message.Block{
				message.ToolUseBlock("call-large", "bash", map[string]any{
					"command": "head -c 5000001 /dev/zero | tr '\\0' x",
				}, nil),
			}, "openai", "gpt-5.4", "", "", 0, nil)),
		}},
		[]provider.StreamEvent{{
			Type: "message_done",
			Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("done", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
		}},
	)
	runtime, err := newAgent(Config{
		Provider:  "openai",
		Model:     "gpt-5.4",
		CWD:       cwd,
		SessionID: "memory-session",
		Tools:     []tools.Spec{tools.Read, tools.Write, tools.Edit, tools.Bash},
	}, adapter)
	if err != nil {
		t.Fatal(err)
	}

	events := collectEvents(runtime.Chat(t.Context(), "run"))

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
}

func TestCompactThresholdBehavior(t *testing.T) {
	for _, tc := range []struct {
		name         string
		threshold    float64
		wantCompact  bool
		wantReqCount int
	}{
		{name: "explicit threshold compacts", threshold: 0.8, wantCompact: true, wantReqCount: 2},
		{name: "zero default disables", wantCompact: false, wantReqCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := openAIAdapter(
				[]provider.StreamEvent{{
					Type: "message_done",
					Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("answer", nil)}, "openai", "gpt-5.4", "", "", 80, nil)),
				}},
				[]provider.StreamEvent{{
					Type: "message_done",
					Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("summary", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
				}},
			)
			runtime, err := newAgent(Config{
				Provider:         "openai",
				Model:            "gpt-5.4",
				CWD:              t.TempDir(),
				Metadata:         &provider.ModelMetadata{ContextWindow: 100},
				CompactThreshold: tc.threshold,
			}, adapter)
			if err != nil {
				t.Fatal(err)
			}

			events := collectEvents(runtime.Chat(t.Context(), "hello"))

			if len(adapter.requests) != tc.wantReqCount {
				t.Fatalf("request count = %d, want %d", len(adapter.requests), tc.wantReqCount)
			}
			if got := hasEvent(events, "compact"); got != tc.wantCompact {
				t.Fatalf("compact event = %v, want %v; events=%#v", got, tc.wantCompact, events)
			}
		})
	}
}

func TestChatSendsTemperature(t *testing.T) {
	cases := []struct {
		name string
		temp *float64
		want *float64
	}{
		{name: "omitted by default", temp: nil, want: nil},
		{name: "sent when set", temp: new(0.2), want: new(0.2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := openAIAdapter([]provider.StreamEvent{{
				Type: "message_done",
				Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("ok", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
			}})
			runtime, err := newAgent(Config{
				Provider:    "openai",
				Model:       "gpt-5.4",
				CWD:         t.TempDir(),
				Temperature: tc.temp,
			}, adapter)
			if err != nil {
				t.Fatal(err)
			}
			collectEvents(runtime.Chat(t.Context(), "hi"))
			if len(adapter.requests) != 1 {
				t.Fatalf("captured %d requests, want 1", len(adapter.requests))
			}
			got := adapter.requests[0].Temperature
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("temperature = %v, want nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("temperature = %v, want %v", got, *tc.want)
			}
		})
	}
}

func TestChatResolvesAttachments(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("hello from file"), 0o644); err != nil {
		t.Fatal(err)
	}
	adapter := openAIAdapter([]provider.StreamEvent{{
		Type: "message_done",
		Msg:  new(message.AssistantMessage([]message.Block{message.TextBlock("ok", nil)}, "openai", "gpt-5.4", "", "", 0, nil)),
	}})
	runtime, err := newAgent(Config{Provider: "openai", Model: "gpt-5.4", CWD: cwd}, adapter)
	if err != nil {
		t.Fatal(err)
	}

	collectEvents(runtime.Chat(t.Context(), "summarize", attachment.Path("notes.txt")))

	if len(adapter.requests) != 1 {
		t.Fatalf("captured %d requests, want 1", len(adapter.requests))
	}
	messages := adapter.requests[0].Messages
	user := messages[len(messages)-1]
	if len(user.Content) != 2 {
		t.Fatalf("user content blocks = %d, want prompt + attachment", len(user.Content))
	}
	if !strings.Contains(user.Content[1].Text, "hello from file") {
		t.Fatalf("attachment block missing file content: %#v", user.Content[1])
	}
}
