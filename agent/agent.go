package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/legibet/mycode-go/attachment"
	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/provider"
	"github.com/legibet/mycode-go/session"
	"github.com/legibet/mycode-go/tools"
)

// Event is one normalized streaming event sent to the API and CLI.
type Event struct {
	Type string
	Data map[string]any
}

// Config describes one agent runtime.
type Config struct {
	// Provider
	Model    string
	Provider string // optional; inferred from Model when empty
	APIKey   string
	APIBase  string

	// Runtime
	CWD              string
	System           string
	Tools            []tools.Spec
	Hooks            tools.Hooks
	MaxTurns         int
	Temperature      *float64
	ReasoningEffort  string
	CompactThreshold float64
	DisableCompact   bool

	// Model capability overrides. Zero/nil resolves from the bundled catalog.
	MaxTokens          int
	ContextWindow      int
	SupportsImageInput *bool
	SupportsPDFInput   *bool

	// Session persistence (optional)
	Store     *session.Store    // nil keeps the run in memory
	SessionID string            // empty generates a random id
	Messages  []message.Message // explicit history; nil auto-resumes from Store; rejected when SessionID already exists
}

// Agent is the single orchestration loop. Construct via New so derived fields
// are filled.
type Agent struct {
	Config
	exec               *tools.Executor
	adapter            provider.Adapter
	transcriptPath     string
	supportsImageInput bool
	supportsPDFInput   bool
}

// New fills defaults and returns a runnable Agent.
func New(cfg Config) (*Agent, error) {
	return newAgent(cfg, nil)
}

// newAgent builds an Agent, optionally with an injected provider adapter so
// tests can drive the loop without a real provider.
func newAgent(cfg Config, adapter provider.Adapter) (*Agent, error) {
	a := &Agent{Config: cfg, adapter: adapter}
	if a.CWD == "" {
		if wd, err := os.Getwd(); err == nil {
			a.CWD = wd
		} else {
			a.CWD = "."
		}
	}
	a.CWD = resolvePath(a.CWD)

	if a.SessionID == "" {
		a.SessionID = randomSessionID()
	}

	// A configured Store turns on persistence: history auto-resumes, every
	// message is appended, and the transcript path feeds the compact prompt.
	toolOutputRoot := filepath.Join(os.TempDir(), "mycode", a.SessionID)
	if a.Store != nil {
		if a.Messages == nil {
			data, err := a.Store.LoadSession(a.SessionID)
			if err != nil {
				return nil, err
			}
			if data != nil {
				a.Messages = data.Messages
			}
		} else if a.Store.SessionExists(a.SessionID) {
			return nil, errors.New("session " + a.SessionID + " already exists on disk; leave Messages nil to resume or choose a different SessionID")
		}
		a.transcriptPath = a.Store.MessagesPath(a.SessionID)
		toolOutputRoot = a.Store.SessionDir(a.SessionID)
	}

	if a.adapter == nil {
		if a.Provider == "" {
			if inferred, ok := provider.InferProviderFromModel(a.Model); ok {
				a.Provider = inferred
			}
		}
		found, ok := provider.LookupAdapter(a.Provider)
		if !ok {
			return nil, errors.New("unsupported provider adapter: " + a.Provider)
		}
		a.adapter = found
	}

	// Resolve capabilities from the bundled catalog; Config fields override it.
	meta := provider.ResolveModel(a.Provider, a.Model, provider.ModelOverride{
		MaxOutputTokens:    a.MaxTokens,
		ContextWindow:      a.ContextWindow,
		SupportsImageInput: a.SupportsImageInput,
		SupportsPDFInput:   a.SupportsPDFInput,
	})
	a.MaxTokens = cmp.Or(meta.MaxOutputTokens, 16384)
	a.ContextWindow = cmp.Or(meta.ContextWindow, 128000)
	a.supportsImageInput = meta.SupportsImageInput
	a.supportsPDFInput = meta.SupportsPDFInput

	a.exec = tools.NewExecutor(a.CWD, toolOutputRoot, a.supportsImageInput)

	if a.DisableCompact {
		a.CompactThreshold = 0
	} else if a.CompactThreshold == 0 {
		a.CompactThreshold = DefaultCompactThreshold
	}
	if a.Temperature != nil {
		if *a.Temperature < 0 || *a.Temperature > 1 {
			return nil, errors.New("temperature must be between 0 and 1")
		}
		// Anthropic-family providers reject a custom temperature while thinking.
		if *a.Temperature != 1 && a.ReasoningEffort != "" && a.ReasoningEffort != "none" {
			switch a.Provider {
			case "anthropic", "moonshotai", "minimax":
				return nil, errors.New(a.Provider + " does not support custom temperature when thinking is enabled")
			}
		}
	}

	a.Messages = message.CloneMessages(a.Messages)
	return a, nil
}

// Cancel stops active tools. Provider cancellation is driven by ctx.
func (a *Agent) Cancel() {
	a.exec.CancelActive()
}

// persist appends one message to the configured store, if any.
func (a *Agent) persist(msg message.Message) error {
	if a.Store == nil {
		return nil
	}
	return a.Store.AppendMessage(a.SessionID, msg, a.CWD)
}

// Chat runs one user turn from a prompt string, optionally with attachments
// resolved against the agent's CWD. It is the convenient entry point; use
// ChatMessage to pass a fully built message (e.g. multi-modal content).
func (a *Agent) Chat(ctx context.Context, prompt string, attachments ...attachment.Attachment) <-chan Event {
	blocks := []message.Block{message.TextBlock(prompt, nil)}
	if len(attachments) > 0 {
		attached, err := attachment.Build(attachments, attachment.Options{CWD: a.CWD})
		if err != nil {
			out := make(chan Event, 1)
			a.emitError(out, err.Error())
			close(out)
			return out
		}
		blocks = append(blocks, attached...)
	}
	return a.ChatMessage(ctx, message.BuildMessage("user", blocks, nil))
}

// ChatMessage runs one user turn from a fully built user message.
func (a *Agent) ChatMessage(ctx context.Context, userInput message.Message) <-chan Event {
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		if userInput.Role != "user" {
			a.emitError(out, "user input must be a user message")
			return
		}
		if err := message.ValidateMediaSupport(userInput, a.supportsImageInput, a.supportsPDFInput); err != nil {
			a.emitError(out, err.Error())
			return
		}

		a.Messages = append(a.Messages, message.Clone(userInput))
		if err := a.persist(userInput); err != nil {
			a.emitError(out, err.Error())
			return
		}

		for turn := 0; a.MaxTurns <= 0 || turn < a.MaxTurns; turn++ {
			assistant, cancelled, err := a.streamAssistantTurn(ctx, out)
			if err != nil {
				a.emitError(out, err.Error())
				return
			}
			if cancelled {
				if assistant != nil {
					a.Messages = append(a.Messages, message.Clone(*assistant))
					if err := a.persist(*assistant); err != nil {
						a.emitError(out, err.Error())
						return
					}
				}
				a.emitError(out, "cancelled")
				return
			}
			if assistant == nil {
				a.emitError(out, "provider produced no assistant message")
				return
			}

			if assistant.Meta == nil {
				assistant.Meta = map[string]any{}
			}
			assistant.Meta["context_window"] = a.ContextWindow
			totalTokens := asInt(assistant.Meta["total_tokens"])

			a.Messages = append(a.Messages, message.Clone(*assistant))
			if err := a.persist(*assistant); err != nil {
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
					if err := a.persist(toolMessage); err != nil {
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
			if a.compactIfNeeded(ctx, out, totalTokens) {
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

func (a *Agent) streamAssistantTurn(ctx context.Context, out chan<- Event) (*message.Message, bool, error) {
	req := provider.Request{
		Provider:           a.Provider,
		Model:              a.Model,
		SessionID:          a.SessionID,
		Messages:           ApplyCompactReplay(a.Messages, a.transcriptPath),
		System:             a.System,
		Tools:              toolSpecs(a.providerToolSpecs()),
		MaxTokens:          a.MaxTokens,
		Temperature:        a.Temperature,
		APIKey:             a.APIKey,
		APIBase:            a.APIBase,
		ReasoningEffort:    a.ReasoningEffort,
		SupportsImageInput: a.supportsImageInput,
		SupportsPDFInput:   a.supportsPDFInput,
	}

	var assistant *message.Message
	var partialContent []message.Block
	var thinkingStartedAt time.Time
	thinkingDurationMs := -1
	for event := range a.adapter.StreamTurn(ctx, req) {
		switch event.Type {
		case "thinking_delta":
			if event.Text != "" {
				if thinkingStartedAt.IsZero() {
					thinkingStartedAt = time.Now()
				}
				if len(partialContent) > 0 && partialContent[len(partialContent)-1].Type == "thinking" {
					partialContent[len(partialContent)-1].Text += event.Text
				} else {
					partialContent = append(partialContent, message.ThinkingBlock(event.Text, nil))
				}
				out <- Event{Type: "reasoning", Data: map[string]any{"delta": event.Text}}
			}
		case "text_delta":
			if event.Text != "" {
				if !thinkingStartedAt.IsZero() && thinkingDurationMs < 0 {
					thinkingDurationMs = max(0, int(time.Since(thinkingStartedAt).Milliseconds()))
					out <- Event{Type: "reasoning_done", Data: map[string]any{"duration_ms": thinkingDurationMs}}
				}
				if len(partialContent) > 0 && partialContent[len(partialContent)-1].Type == "text" {
					partialContent[len(partialContent)-1].Text += event.Text
				} else {
					partialContent = append(partialContent, message.TextBlock(event.Text, nil))
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
			if ctx.Err() != nil {
				return a.cancelledAssistantMessage(partialContent, thinkingStartedAt, thinkingDurationMs), true, nil
			}
			if event.Err != nil {
				return nil, false, event.Err
			}
			return nil, false, errors.New("provider error")
		}
	}
	if ctx.Err() != nil {
		return a.cancelledAssistantMessage(partialContent, thinkingStartedAt, thinkingDurationMs), true, nil
	}
	if assistant == nil {
		return nil, false, errors.New("provider produced no assistant message")
	}

	applyThinkingDuration(assistant.Content, thinkingDurationMs)
	return assistant, false, nil
}

func (a *Agent) cancelledAssistantMessage(blocks []message.Block, thinkingStartedAt time.Time, thinkingDurationMs int) *message.Message {
	if len(blocks) == 0 {
		return nil
	}
	if !thinkingStartedAt.IsZero() && thinkingDurationMs < 0 {
		thinkingDurationMs = max(0, int(time.Since(thinkingStartedAt).Milliseconds()))
	}
	applyThinkingDuration(blocks, thinkingDurationMs)

	return new(message.BuildMessage("assistant", blocks, map[string]any{
		"provider":       a.Provider,
		"model":          a.Model,
		"context_window": a.ContextWindow,
	}))
}

func applyThinkingDuration(blocks []message.Block, durationMs int) {
	if durationMs < 0 {
		return
	}
	for i, block := range slices.Backward(blocks) {
		if block.Type != "thinking" {
			continue
		}
		if blocks[i].Meta == nil {
			blocks[i].Meta = map[string]any{}
		}
		blocks[i].Meta["duration_ms"] = durationMs
		return
	}
}

func (a *Agent) executeToolCalls(ctx context.Context, toolCalls []message.Block, out chan<- Event) (message.Message, bool) {
	toolResults := make([]message.Block, 0, len(toolCalls))
	appendResult := func(toolCall message.Block, result tools.Result) {
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
	}

	for _, toolCall := range toolCalls {
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
		if ctx.Err() != nil {
			appendResult(toolCall, tools.Result{Output: "error: cancelled", IsError: true})
			return message.BuildMessage("user", toolResults, nil), true
		}

		var result tools.Result
		call := a.toolCall(toolCall, input, nil)
		if hookResult, handled := a.runBeforeToolHooks(ctx, call); handled {
			result = hookResult
		} else if spec, ok := a.lookupToolSpec(toolCall.Name); ok && spec.Runner != nil {
			result = a.runToolSpec(ctx, toolCall, spec, out)
		} else {
			result = tools.Result{Output: "error: unknown tool: " + toolCall.Name, IsError: true}
		}
		result = a.runAfterToolHooks(ctx, call, result)
		appendResult(toolCall, result)

		if result.IsError && ctx.Err() != nil && strings.HasSuffix(result.Output, "error: cancelled") {
			return message.BuildMessage("user", toolResults, nil), true
		}
	}

	return message.BuildMessage("user", toolResults, nil), false
}

func randomSessionID() string {
	var data [8]byte
	if _, err := rand.Read(data[:]); err == nil {
		return hex.EncodeToString(data[:])
	}
	return "session-" + strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339Nano), ":", "")
}

func (a *Agent) lookupToolSpec(name string) (tools.Spec, bool) {
	for _, spec := range a.providerToolSpecs() {
		if spec.Name == name {
			return spec, true
		}
	}
	return tools.Spec{}, false
}

func (a *Agent) runBeforeToolHooks(ctx context.Context, call tools.ToolCall) (tools.Result, bool) {
	for _, hook := range a.Hooks.BeforeTool {
		if result, handled := hook(ctx, call); handled {
			return result, true
		}
	}
	return tools.Result{}, false
}

func (a *Agent) runAfterToolHooks(ctx context.Context, call tools.ToolCall, result tools.Result) tools.Result {
	for _, hook := range a.Hooks.AfterTool {
		if next, replaced := hook(ctx, call, result); replaced {
			result = next
		}
	}
	return result
}

func (a *Agent) runToolSpec(ctx context.Context, toolCall message.Block, spec tools.Spec, out chan<- Event) tools.Result {
	var emitted []string
	var emit tools.OutputCallback
	if spec.StreamsOutput {
		emit = func(text string) {
			if ctx.Err() != nil {
				return
			}
			emitted = append(emitted, text)
			out <- Event{Type: "tool_output", Data: map[string]any{
				"tool_use_id": toolCall.ID,
				"output":      text,
			}}
		}
	}
	result := a.exec.Run(ctx, spec, toolCall.ID, toolCall.Input, emit)
	// A cancelled streaming tool returns the output emitted so far followed by
	// the cancellation marker.
	if spec.StreamsOutput && ctx.Err() != nil && result.Output == "error: cancelled" && len(emitted) > 0 {
		result.Output = strings.Join(append(emitted, "error: cancelled"), "\n")
	}
	return result
}

func (a *Agent) toolCall(toolCall message.Block, input map[string]any, emit tools.OutputCallback) tools.ToolCall {
	return tools.ToolCall{
		ID:    toolCall.ID,
		Name:  toolCall.Name,
		Input: maps.Clone(input),
		CWD:   a.CWD,
		Emit:  emit,
	}
}

func (a *Agent) emitError(out chan<- Event, msg string) {
	out <- Event{Type: "error", Data: map[string]any{"message": msg}}
}

// compactIfNeeded triggers compaction when the latest turn crosses the
// threshold. Returns true to stop the agent loop (cancellation). Provider
// failures during compaction are swallowed — the next turn either retries or
// surfaces the error from phase 1.
func (a *Agent) compactIfNeeded(ctx context.Context, out chan<- Event, totalTokens int) bool {
	if !ShouldCompact(totalTokens, a.ContextWindow, a.CompactThreshold) {
		return false
	}

	compactMessages := slices.Concat(
		ApplyCompactReplay(a.Messages, a.transcriptPath),
		[]message.Message{message.UserTextMessage(CompactSummaryPrompt, nil)},
	)
	req := provider.Request{
		Provider:           a.Provider,
		Model:              a.Model,
		SessionID:          a.SessionID,
		Messages:           compactMessages,
		System:             a.System,
		MaxTokens:          min(a.MaxTokens, 8192),
		Temperature:        a.Temperature,
		APIKey:             a.APIKey,
		APIBase:            a.APIBase,
		SupportsImageInput: a.supportsImageInput,
		SupportsPDFInput:   a.supportsPDFInput,
	}

	var summary *message.Message
	for event := range a.adapter.StreamTurn(ctx, req) {
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
	if err := a.persist(compactEvent); err != nil {
		return false
	}
	a.Messages = append(a.Messages, compactEvent)
	out <- Event{Type: "compact", Data: map[string]any{}}
	return false
}

func (a *Agent) providerToolSpecs() []tools.Spec {
	return a.Tools
}

func toolSpecs(specs []tools.Spec) []map[string]any {
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

func resolvePath(path string) string {
	if path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(absolute)
}
