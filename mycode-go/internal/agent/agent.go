package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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
	"github.com/legibet/mycode-go/internal/session"
	"github.com/legibet/mycode-go/internal/tools"
)

// Event is the normalized streaming event sent to the API and CLI.
type Event struct {
	Type string
	Data map[string]any
}

// PersistFunc stores one canonical message.
type PersistFunc func(message.Message) error

// Options configures one agent instance.
type Options struct {
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
}

// Agent is the single orchestration loop.
type Agent struct {
	Model              string
	Provider           string
	CWD                string
	Project            string
	SessionDir         string
	SessionID          string
	APIKey             string
	APIBase            string
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
	System             string
	Messages           []message.Message
	Tools              *tools.Executor
	Adapter            provider.Adapter
}

// New creates an agent from options.
func New(opts Options) (*Agent, error) {
	cwd := opts.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			cwd = "."
		}
	}
	if absolute, err := filepath.Abs(cwd); err == nil {
		cwd = absolute
	}

	sessionDir := cmp.Or(opts.SessionDir, cwd)
	sessionID := cmp.Or(opts.SessionID, filepath.Base(sessionDir))
	permission := opts.Permission
	if permission.Level == "" {
		permission = config.DefaultPermissionConfig()
	}
	project := cmp.Or(opts.Project, config.ResolveProject(cwd))

	skillRoots := append([]string(nil), opts.SkillRoots...)
	if skillRoots == nil {
		skillRoots = permissions.SkillRoots(cwd, project, config.ResolveHome())
	}

	toolExecutor := opts.Tools
	if toolExecutor == nil {
		toolExecutor = tools.NewExecutor(cwd, sessionDir, opts.SupportsImageInput)
	}

	adapter := opts.Adapter
	if adapter == nil {
		var ok bool
		adapter, ok = provider.LookupAdapter(opts.Provider)
		if !ok {
			return nil, errors.New("unsupported provider adapter: " + opts.Provider)
		}
	}

	system := opts.System
	if system == "" {
		system = prompt.Build(cwd, project, config.ResolveHome())
	}

	return &Agent{
		Model:              opts.Model,
		Provider:           opts.Provider,
		CWD:                cwd,
		Project:            project,
		SessionDir:         sessionDir,
		SessionID:          sessionID,
		APIKey:             opts.APIKey,
		APIBase:            opts.APIBase,
		MaxTurns:           opts.MaxTurns,
		MaxTokens:          opts.MaxTokens,
		ContextWindow:      opts.ContextWindow,
		CompactThreshold:   opts.CompactThreshold,
		ReasoningEffort:    opts.ReasoningEffort,
		SupportsImageInput: opts.SupportsImageInput,
		SupportsPDFInput:   opts.SupportsPDFInput,
		Permission:         permission,
		PermissionReviewer: opts.PermissionReviewer,
		SkillRoots:         skillRoots,
		System:             system,
		Messages:           message.CloneMessages(opts.Messages),
		Tools:              toolExecutor,
		Adapter:            adapter,
	}, nil
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
		if err := a.validateUserInput(userInput); err != nil {
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

			a.Messages = append(a.Messages, message.Clone(*assistant))
			if err := persist(*assistant); err != nil {
				a.emitError(out, err.Error())
				return
			}

			toolCalls := make([]message.Block, 0, len(assistant.Content))
			for _, block := range assistant.Content {
				if block.Type == "tool_use" {
					toolCalls = append(toolCalls, block)
				}
			}
			if len(toolCalls) == 0 {
				a.compactIfNeeded(ctx, onPersist, out)
				return
			}

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

		a.emitError(out, "max_turns reached")
	}()
	return out
}

func (a *Agent) streamAssistantTurn(ctx context.Context, out chan<- Event) (*message.Message, error) {
	req := provider.Request{
		Provider:           a.Provider,
		Model:              a.Model,
		SessionID:          a.SessionID,
		Messages:           a.Messages,
		System:             a.System,
		Tools:              toolSpecs(a.Tools.Definitions()),
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

func (a *Agent) validateUserInput(userInput message.Message) error {
	for _, block := range userInput.Content {
		if block.Type == "image" && !a.SupportsImageInput {
			return fmt.Errorf("current model does not support image input")
		}
		if block.Type == "document" && !a.SupportsPDFInput {
			return fmt.Errorf("current model does not support PDF input")
		}
	}
	return nil
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

func (a *Agent) compactIfNeeded(ctx context.Context, onPersist PersistFunc, out chan<- Event) {
	if len(a.Messages) == 0 {
		return
	}

	var usage map[string]any
	for i := len(a.Messages) - 1; i >= 0; i-- {
		if a.Messages[i].Role == "assistant" {
			usage, _ = a.Messages[i].Meta["usage"].(map[string]any)
			break
		}
	}
	if !session.ShouldCompact(usage, a.ContextWindow, a.CompactThreshold) {
		return
	}

	beforeCount := len(a.Messages)
	compactMessages := append(slices.Clone(a.Messages), message.UserTextMessage(session.CompactSummaryPrompt, nil))
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
		if event.Type == "provider_error" {
			return
		}
	}
	if ctx.Err() != nil || summary == nil {
		return
	}
	summaryText := message.FlattenText(*summary, false)
	if summaryText == "" {
		return
	}
	compactEvent := session.BuildCompactEvent(summaryText, a.Provider, a.Model, beforeCount, summary.Meta["usage"])
	if onPersist != nil {
		if err := onPersist(compactEvent); err != nil {
			return
		}
	}
	a.Messages = append(a.Messages, compactEvent)
	a.Messages = session.ApplyCompact(a.Messages)
	out <- Event{Type: "compact", Data: map[string]any{
		"message":         fmt.Sprintf("Context compacted (%d messages -> summary)", beforeCount),
		"compacted_count": beforeCount,
	}}
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
