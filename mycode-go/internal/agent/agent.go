package agent

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/legibet/mycode-go/internal/config"
	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/permissions"
	"github.com/legibet/mycode-go/internal/prompt"
	"github.com/legibet/mycode-go/internal/provider"
	"github.com/legibet/mycode-go/internal/tools"
)

// Event is one normalized streaming event sent to the API and CLI.
type Event struct {
	Type string
	Data map[string]any
}

// PersistFunc stores one canonical message.
type PersistFunc func(message.Message) error

// Agent is the single orchestration loop. Construct via New so derived fields
// (TranscriptPath, default Tools/Adapter, default System prompt) are filled.
type Agent struct {
	Model              string
	Provider           string
	CWD                string
	Project            string
	SessionDir         string
	SessionID          string
	APIKey             string
	APIBase            string
	System             string
	Messages           []message.Message
	MaxTurns           int
	MaxTokens          int
	ContextWindow      int
	CompactThreshold   float64
	ReasoningEffort    string
	SupportsImageInput bool
	SupportsPDFInput   bool
	Permission         config.PermissionConfig
	PermissionReviewer permissions.Reviewer
	SkillRoots         []string
	Adapter            provider.Adapter
	Tools              *tools.Executor

	// TranscriptPath is set by New from SessionDir.
	TranscriptPath string
}

// New fills defaults and returns a runnable Agent.
func New(a Agent) (*Agent, error) {
	if a.CWD == "" {
		if wd, err := os.Getwd(); err == nil {
			a.CWD = wd
		} else {
			a.CWD = "."
		}
	}
	if absolute, err := filepath.Abs(a.CWD); err == nil {
		a.CWD = absolute
	}

	// Without an explicit SessionDir, skip the "read the original log at:"
	// hint in the compact projection (Python's transcript_path=None branch).
	explicitSessionDir := a.SessionDir
	a.SessionDir = cmp.Or(a.SessionDir, a.CWD)
	a.SessionID = cmp.Or(a.SessionID, filepath.Base(a.SessionDir))
	if a.TranscriptPath == "" && explicitSessionDir != "" {
		a.TranscriptPath = filepath.Join(explicitSessionDir, "messages.jsonl")
	}

	if a.Permission.Level == "" {
		a.Permission = config.DefaultPermissionConfig()
	}
	a.Project = cmp.Or(a.Project, config.ResolveProject(a.CWD))

	if a.SkillRoots == nil {
		a.SkillRoots = permissions.SkillRoots(a.CWD, a.Project, config.ResolveHome())
	} else {
		a.SkillRoots = slices.Clone(a.SkillRoots)
	}

	if a.Tools == nil {
		a.Tools = tools.NewExecutor(a.CWD, a.SessionDir, a.SupportsImageInput)
	}

	if a.Adapter == nil {
		adapter, ok := provider.LookupAdapter(a.Provider)
		if !ok {
			return nil, errors.New("unsupported provider adapter: " + a.Provider)
		}
		a.Adapter = adapter
	}

	if a.System == "" {
		a.System = prompt.Build(a.CWD, a.Project, config.ResolveHome())
	}
	if a.ContextWindow == 0 {
		a.ContextWindow = 128000
	}

	a.Messages = message.CloneMessages(a.Messages)
	return &a, nil
}

// Cancel stops active tools. Provider cancellation is driven by ctx.
func (a *Agent) Cancel() {
	a.Tools.CancelActive()
}

// Chat runs one user turn.
func (a *Agent) Chat(ctx context.Context, userInput message.Message, onPersist PersistFunc) <-chan Event {
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		if userInput.Role != "user" {
			a.emitError(out, "user input must be a user message")
			return
		}
		if err := message.ValidateMediaSupport(userInput, a.SupportsImageInput, a.SupportsPDFInput); err != nil {
			a.emitError(out, err.Error())
			return
		}

		persist := func(msg message.Message) error {
			if onPersist == nil {
				return nil
			}
			return onPersist(msg)
		}

		a.Messages = append(a.Messages, message.Clone(userInput))
		if err := persist(userInput); err != nil {
			a.emitError(out, err.Error())
			return
		}

		for turn := 0; a.MaxTurns <= 0 || turn < a.MaxTurns; turn++ {
			assistant, err := a.streamAssistantTurn(ctx, out)
			if err != nil {
				if ctx.Err() != nil {
					a.emitError(out, "cancelled")
				} else {
					a.emitError(out, err.Error())
				}
				return
			}

			if assistant.Meta == nil {
				assistant.Meta = map[string]any{}
			}
			assistant.Meta["context_window"] = a.ContextWindow
			totalTokens := asInt(assistant.Meta["total_tokens"])

			a.Messages = append(a.Messages, message.Clone(*assistant))
			if err := persist(*assistant); err != nil {
				a.emitError(out, err.Error())
				return
			}
			if totalTokens > 0 {
				out <- Event{Type: "usage", Data: map[string]any{
					"total_tokens":   totalTokens,
					"model":          cmp.Or(asString(assistant.Meta["model"]), a.Model),
					"provider":       cmp.Or(asString(assistant.Meta["provider"]), a.Provider),
					"context_window": a.ContextWindow,
				}}
			}

			toolCalls := make([]message.Block, 0, len(assistant.Content))
			for _, block := range assistant.Content {
				if block.Type == "tool_use" {
					toolCalls = append(toolCalls, block)
				}
			}
			if len(toolCalls) > 0 {
				toolMessage, cancelled := a.executeToolCalls(ctx, toolCalls, out)
				if len(toolMessage.Content) > 0 {
					a.Messages = append(a.Messages, toolMessage)
					if err := persist(toolMessage); err != nil {
						a.emitError(out, err.Error())
						return
					}
				}
				if cancelled {
					if len(toolMessage.Content) == 0 {
						a.emitError(out, "cancelled")
					}
					return
				}
			}
			if ctx.Err() != nil {
				return
			}
			if a.compactIfNeeded(ctx, persist, out, totalTokens) {
				return
			}
			if len(toolCalls) == 0 {
				return
			}
		}

		a.emitError(out, "max_turns reached")
	}()
	return out
}

func (a *Agent) streamAssistantTurn(ctx context.Context, out chan<- Event) (*message.Message, error) {
	req := provider.Request{
		Provider:           a.Provider,
		Model:              a.Model,
		SessionID:          a.SessionID,
		Messages:           ApplyCompactReplay(a.Messages, a.TranscriptPath),
		System:             a.System,
		Tools:              toolSpecs(tools.DefaultSpecs()),
		MaxTokens:          a.MaxTokens,
		APIKey:             a.APIKey,
		APIBase:            a.APIBase,
		ReasoningEffort:    a.ReasoningEffort,
		SupportsImageInput: a.SupportsImageInput,
		SupportsPDFInput:   a.SupportsPDFInput,
	}

	var assistant *message.Message
	var thinkingStartedAt time.Time
	thinkingDurationMs := -1
	for event := range a.Adapter.StreamTurn(ctx, req) {
		switch event.Type {
		case "thinking_delta":
			if event.Text != "" {
				if thinkingStartedAt.IsZero() {
					thinkingStartedAt = time.Now()
				}
				out <- Event{Type: "reasoning", Data: map[string]any{"delta": event.Text}}
			}
		case "text_delta":
			if event.Text != "" {
				if !thinkingStartedAt.IsZero() && thinkingDurationMs < 0 {
					thinkingDurationMs = max(0, int(time.Since(thinkingStartedAt).Milliseconds()))
					out <- Event{Type: "reasoning_done", Data: map[string]any{"duration_ms": thinkingDurationMs}}
				}
				out <- Event{Type: "text", Data: map[string]any{"delta": event.Text}}
			}
		case "message_done":
			if !thinkingStartedAt.IsZero() && thinkingDurationMs < 0 {
				thinkingDurationMs = max(0, int(time.Since(thinkingStartedAt).Milliseconds()))
				out <- Event{Type: "reasoning_done", Data: map[string]any{"duration_ms": thinkingDurationMs}}
			}
			assistant = event.Msg
		case "provider_error":
			if event.Err != nil {
				return nil, event.Err
			}
			return nil, errors.New("provider error")
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if assistant == nil {
		return nil, errors.New("provider produced no assistant message")
	}

	if thinkingDurationMs >= 0 {
		for i := len(assistant.Content) - 1; i >= 0; i-- {
			if assistant.Content[i].Type != "thinking" {
				continue
			}
			if assistant.Content[i].Meta == nil {
				assistant.Content[i].Meta = map[string]any{}
			}
			assistant.Content[i].Meta["duration_ms"] = thinkingDurationMs
			break
		}
	}

	return assistant, nil
}

func (a *Agent) executeToolCalls(ctx context.Context, toolCalls []message.Block, out chan<- Event) (message.Message, bool) {
	toolResults := make([]message.Block, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if ctx.Err() != nil {
			return message.Message{}, true
		}

		input := maps.Clone(toolCall.Input)
		if input == nil {
			input = map[string]any{}
		}
		out <- Event{Type: "tool_start", Data: map[string]any{
			"tool_call": map[string]any{
				"id":    toolCall.ID,
				"name":  toolCall.Name,
				"input": input,
			},
		}}

		toolCall.Input = input
		result, stop := a.authorizeTool(ctx, toolCall, input)
		if result == nil {
			runResult := a.runTool(toolCall, out)
			result = &runResult
		}
		toolResults = append(toolResults, message.ToolResultBlock(
			toolCall.ID,
			result.Output,
			result.Metadata,
			result.IsError,
			result.Content,
			nil,
		))

		data := map[string]any{
			"tool_use_id": toolCall.ID,
			"output":      result.Output,
			"is_error":    result.IsError,
		}
		if len(result.Metadata) > 0 {
			data["metadata"] = result.Metadata
		}
		if len(result.Content) > 0 {
			data["content"] = result.Content
		}
		out <- Event{Type: "tool_done", Data: data}

		if stop {
			return message.BuildMessage("user", toolResults, nil), true
		}
		if result.Output == "error: cancelled" && ctx.Err() != nil {
			return message.BuildMessage("user", toolResults, nil), true
		}
	}

	return message.BuildMessage("user", toolResults, nil), false
}

func (a *Agent) authorizeTool(ctx context.Context, toolCall message.Block, input map[string]any) (*tools.Result, bool) {
	check := permissions.ClassifyTool(toolCall.Name, input, a.CWD, a.Project, a.SkillRoots)
	switch permissions.DecisionFor(a.Permission, check.Tier) {
	case permissions.DecisionAllow:
		return nil, false
	case permissions.DecisionDeny:
		return &tools.Result{Output: permissions.DeniedOutput, IsError: true}, false
	default:
		if a.PermissionReviewer == nil {
			return &tools.Result{Output: permissions.DeniedOutput, IsError: true}, false
		}
		decision := a.PermissionReviewer(ctx, permissions.ReviewRequest{
			ToolCallID: toolCall.ID,
			ToolName:   toolCall.Name,
			Preview:    check.Preview,
		})
		if decision == permissions.ReviewAllow {
			return nil, false
		}
		return &tools.Result{Output: permissions.DeniedByUserOutput, IsError: true}, true
	}
}

func (a *Agent) emitError(out chan<- Event, msg string) {
	out <- Event{Type: "error", Data: map[string]any{"message": msg}}
}

func (a *Agent) runTool(toolCall message.Block, out chan<- Event) tools.Result {
	switch toolCall.Name {
	case "read":
		return a.Tools.Read(asString(toolCall.Input["path"]), asInt(toolCall.Input["offset"]), asInt(toolCall.Input["limit"]))
	case "write":
		return a.Tools.Write(asString(toolCall.Input["path"]), asString(toolCall.Input["content"]))
	case "edit":
		return a.Tools.Edit(asString(toolCall.Input["path"]), asEdits(toolCall.Input["edits"]))
	case "bash":
		return a.Tools.Bash(toolCall.ID, asString(toolCall.Input["command"]), asInt(toolCall.Input["timeout"]), func(text string) {
			out <- Event{Type: "tool_output", Data: map[string]any{
				"tool_use_id": toolCall.ID,
				"output":      text,
			}}
		})
	default:
		return tools.Result{Output: "error: unknown tool: " + toolCall.Name, IsError: true}
	}
}

// compactIfNeeded triggers compaction when the latest turn crosses the
// threshold. Returns true to stop the agent loop (cancellation). Provider
// failures during compaction are swallowed — the next turn either retries or
// surfaces the error from phase 1.
func (a *Agent) compactIfNeeded(ctx context.Context, persist PersistFunc, out chan<- Event, totalTokens int) bool {
	if !ShouldCompact(totalTokens, a.ContextWindow, a.CompactThreshold) {
		return false
	}

	compactMessages := slices.Concat(
		ApplyCompactReplay(a.Messages, a.TranscriptPath),
		[]message.Message{message.UserTextMessage(CompactSummaryPrompt, nil)},
	)
	req := provider.Request{
		Provider:           a.Provider,
		Model:              a.Model,
		SessionID:          a.SessionID,
		Messages:           compactMessages,
		System:             a.System,
		MaxTokens:          min(a.MaxTokens, 8192),
		APIKey:             a.APIKey,
		APIBase:            a.APIBase,
		SupportsImageInput: a.SupportsImageInput,
		SupportsPDFInput:   a.SupportsPDFInput,
	}

	var summary *message.Message
	for event := range a.Adapter.StreamTurn(ctx, req) {
		if event.Type == "message_done" {
			summary = event.Msg
		}
	}
	if ctx.Err() != nil {
		a.emitError(out, "cancelled")
		return true
	}
	if summary == nil {
		return false
	}
	summaryText := message.FlattenText(*summary, false)
	if summaryText == "" {
		return false
	}
	compactEvent := BuildCompactEvent(summaryText, a.Provider, a.Model, asInt(summary.Meta["total_tokens"]))
	if err := persist(compactEvent); err != nil {
		return false
	}
	a.Messages = append(a.Messages, compactEvent)
	out <- Event{Type: "compact", Data: map[string]any{}}
	return false
}

func toolSpecs(specs []tools.ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		out = append(out, map[string]any{
			"name":         spec.Name,
			"description":  spec.Description,
			"input_schema": spec.InputSchema,
		})
	}
	return out
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}

func asInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case int32:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func asEdits(value any) []map[string]string {
	items, _ := value.([]any)
	out := make([]map[string]string, 0, len(items))
	for _, item := range items {
		entry, _ := item.(map[string]any)
		if entry == nil {
			continue
		}
		out = append(out, map[string]string{
			"oldText": asString(entry["oldText"]),
			"newText": asString(entry["newText"]),
		})
	}
	return out
}
