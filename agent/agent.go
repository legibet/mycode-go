package agent

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"iter"
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
	MaxOutputTokens    int
	ContextWindow      int
	SupportsImageInput *bool
	SupportsPDFInput   *bool

	// Session persistence (optional)
	Store     *session.Store    // nil keeps the run in memory
	SessionID string            // empty generates a random id
	Messages  []message.Message // explicit history; nil auto-resumes from Store; rejected when SessionID already exists
}

// Agent is the single orchestration loop. Construct via New.
type Agent struct {
	// cfg is a private copy; newAgent fills defaults and resolved values into
	// it, so callers never observe field rewrites.
	cfg                Config
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
	a := &Agent{cfg: cfg, adapter: adapter}
	if a.cfg.CWD == "" {
		if wd, err := os.Getwd(); err == nil {
			a.cfg.CWD = wd
		} else {
			a.cfg.CWD = "."
		}
	}
	a.cfg.CWD = resolvePath(a.cfg.CWD)

	if a.cfg.SessionID == "" {
		a.cfg.SessionID = randomSessionID()
	}

	// A configured Store turns on persistence: history auto-resumes, every
	// message is appended, and the transcript path feeds the compact prompt.
	toolOutputRoot := filepath.Join(os.TempDir(), "mycode", a.cfg.SessionID)
	if a.cfg.Store != nil {
		if a.cfg.Messages == nil {
			data, err := a.cfg.Store.LoadSession(a.cfg.SessionID)
			if err != nil {
				return nil, err
			}
			if data != nil {
				a.cfg.Messages = data.Messages
			}
		} else if a.cfg.Store.SessionExists(a.cfg.SessionID) {
			return nil, errors.New("session " + a.cfg.SessionID + " already exists on disk; leave Messages nil to resume or choose a different SessionID")
		}
		a.transcriptPath = a.cfg.Store.MessagesPath(a.cfg.SessionID)
		toolOutputRoot = a.cfg.Store.SessionDir(a.cfg.SessionID)
	}

	if a.adapter == nil {
		if a.cfg.Provider == "" {
			if inferred, ok := provider.InferProviderFromModel(a.cfg.Model); ok {
				a.cfg.Provider = inferred
			}
		}
		found, ok := provider.LookupAdapter(a.cfg.Provider)
		if !ok {
			return nil, errors.New("unsupported provider adapter: " + a.cfg.Provider)
		}
		a.adapter = found
	}

	// Resolve capabilities from the bundled catalog; Config fields override it.
	meta := provider.ResolveModel(a.cfg.Provider, a.cfg.Model, provider.ModelOverride{
		MaxOutputTokens:    a.cfg.MaxOutputTokens,
		ContextWindow:      a.cfg.ContextWindow,
		SupportsImageInput: a.cfg.SupportsImageInput,
		SupportsPDFInput:   a.cfg.SupportsPDFInput,
	})
	a.cfg.MaxOutputTokens = cmp.Or(meta.MaxOutputTokens, 16384)
	a.cfg.ContextWindow = cmp.Or(meta.ContextWindow, 128000)
	a.supportsImageInput = meta.SupportsImageInput
	a.supportsPDFInput = meta.SupportsPDFInput

	a.exec = tools.NewExecutor(a.cfg.CWD, toolOutputRoot, a.supportsImageInput)

	if a.cfg.DisableCompact {
		a.cfg.CompactThreshold = 0
	} else if a.cfg.CompactThreshold == 0 {
		a.cfg.CompactThreshold = DefaultCompactThreshold
	}
	if a.cfg.Temperature != nil {
		if *a.cfg.Temperature < 0 || *a.cfg.Temperature > 1 {
			return nil, errors.New("temperature must be between 0 and 1")
		}
		// Anthropic-family providers reject a custom temperature while thinking.
		if *a.cfg.Temperature != 1 && a.cfg.ReasoningEffort != "" && a.cfg.ReasoningEffort != "none" {
			switch a.cfg.Provider {
			case "anthropic", "moonshotai", "minimax":
				return nil, errors.New(a.cfg.Provider + " does not support custom temperature when thinking is enabled")
			}
		}
	}

	a.cfg.Messages = message.CloneMessages(a.cfg.Messages)
	return a, nil
}

// SessionID returns the session id, generated when Config left it empty.
func (a *Agent) SessionID() string {
	return a.cfg.SessionID
}

// Messages returns a copy of the conversation history accumulated so far.
func (a *Agent) Messages() []message.Message {
	return message.CloneMessages(a.cfg.Messages)
}

// Cancel stops active tools. Provider cancellation is driven by ctx.
func (a *Agent) Cancel() {
	a.exec.CancelActive()
}

// persist appends one message to the configured store, if any.
func (a *Agent) persist(msg message.Message) error {
	if a.cfg.Store == nil {
		return nil
	}
	return a.cfg.Store.AppendMessage(a.cfg.SessionID, msg, a.cfg.CWD)
}

// emitter bridges the loop's event pushes to the consumer's pull. Once the
// consumer stops iterating, send drops further events and stopped stays true
// so the loop can unwind without calling yield again.
type emitter struct {
	yield   func(Event) bool
	stopped bool
}

func (e *emitter) send(event Event) {
	if !e.stopped {
		e.stopped = !e.yield(event)
	}
}

func (e *emitter) sendError(msg string) {
	e.send(Event{Type: "error", Data: map[string]any{"message": msg}})
}

// Chat runs one user turn from a prompt string, optionally with attachments
// resolved against the agent's CWD. It is the convenient entry point; use
// ChatMessage to pass a fully built message (e.g. multi-modal content).
func (a *Agent) Chat(ctx context.Context, prompt string, attachments ...attachment.Attachment) iter.Seq[Event] {
	blocks := []message.Block{message.TextBlock(prompt, nil)}
	if len(attachments) > 0 {
		attached, err := attachment.Build(attachments, attachment.Options{CWD: a.cfg.CWD})
		if err != nil {
			return func(yield func(Event) bool) {
				yield(Event{Type: "error", Data: map[string]any{"message": err.Error()}})
			}
		}
		blocks = append(blocks, attached...)
	}
	return a.ChatMessage(ctx, message.BuildMessage("user", blocks, nil))
}

// ChatMessage runs one user turn from a fully built user message. The turn
// loops provider → tool calls → provider until the assistant stops calling
// tools. Breaking out of the iteration stops the loop at the next phase
// boundary; cancel ctx to interrupt a provider stream or running tool.
func (a *Agent) ChatMessage(ctx context.Context, userInput message.Message) iter.Seq[Event] {
	return func(yield func(Event) bool) {
		out := &emitter{yield: yield}
		if userInput.Role != "user" {
			out.sendError("user input must be a user message")
			return
		}
		if err := message.ValidateMediaSupport(userInput, a.supportsImageInput, a.supportsPDFInput); err != nil {
			out.sendError(err.Error())
			return
		}

		a.cfg.Messages = append(a.cfg.Messages, message.Clone(userInput))
		if err := a.persist(userInput); err != nil {
			out.sendError(err.Error())
			return
		}

		for turn := 0; a.cfg.MaxTurns <= 0 || turn < a.cfg.MaxTurns; turn++ {
			assistant, cancelled, err := a.streamAssistantTurn(ctx, out)
			if err != nil {
				out.sendError(err.Error())
				return
			}
			if cancelled {
				if assistant != nil {
					a.cfg.Messages = append(a.cfg.Messages, message.Clone(*assistant))
					if err := a.persist(*assistant); err != nil {
						out.sendError(err.Error())
						return
					}
				}
				out.sendError("cancelled")
				return
			}
			if assistant == nil {
				out.sendError("provider produced no assistant message")
				return
			}

			if assistant.Meta == nil {
				assistant.Meta = map[string]any{}
			}
			assistant.Meta["context_window"] = a.cfg.ContextWindow
			totalTokens := asInt(assistant.Meta["total_tokens"])

			a.cfg.Messages = append(a.cfg.Messages, message.Clone(*assistant))
			if err := a.persist(*assistant); err != nil {
				out.sendError(err.Error())
				return
			}
			if totalTokens > 0 {
				out.send(Event{Type: "usage", Data: map[string]any{
					"total_tokens":   totalTokens,
					"model":          cmp.Or(asString(assistant.Meta["model"]), a.cfg.Model),
					"provider":       cmp.Or(asString(assistant.Meta["provider"]), a.cfg.Provider),
					"context_window": a.cfg.ContextWindow,
				}})
			}
			if out.stopped {
				return
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
					a.cfg.Messages = append(a.cfg.Messages, toolMessage)
					if err := a.persist(toolMessage); err != nil {
						out.sendError(err.Error())
						return
					}
				}
				if cancelled {
					if len(toolMessage.Content) == 0 {
						out.sendError("cancelled")
					}
					return
				}
			}
			if ctx.Err() != nil || out.stopped {
				return
			}
			if a.compactIfNeeded(ctx, out, totalTokens) {
				return
			}
			if len(toolCalls) == 0 {
				return
			}
		}

		out.sendError("max_turns reached")
	}
}

func (a *Agent) streamAssistantTurn(ctx context.Context, out *emitter) (*message.Message, bool, error) {
	req := provider.Request{
		Provider:           a.cfg.Provider,
		Model:              a.cfg.Model,
		SessionID:          a.cfg.SessionID,
		Messages:           applyCompactReplay(a.cfg.Messages, a.transcriptPath),
		System:             a.cfg.System,
		Tools:              providerTools(a.cfg.Tools),
		MaxTokens:          a.cfg.MaxOutputTokens,
		Temperature:        a.cfg.Temperature,
		APIKey:             a.cfg.APIKey,
		APIBase:            a.cfg.APIBase,
		ReasoningEffort:    a.cfg.ReasoningEffort,
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
				out.send(Event{Type: "reasoning", Data: map[string]any{"delta": event.Text}})
			}
		case "text_delta":
			if event.Text != "" {
				if !thinkingStartedAt.IsZero() && thinkingDurationMs < 0 {
					thinkingDurationMs = max(0, int(time.Since(thinkingStartedAt).Milliseconds()))
					out.send(Event{Type: "reasoning_done", Data: map[string]any{"duration_ms": thinkingDurationMs}})
				}
				if len(partialContent) > 0 && partialContent[len(partialContent)-1].Type == "text" {
					partialContent[len(partialContent)-1].Text += event.Text
				} else {
					partialContent = append(partialContent, message.TextBlock(event.Text, nil))
				}
				out.send(Event{Type: "text", Data: map[string]any{"delta": event.Text}})
			}
		case "message_done":
			if !thinkingStartedAt.IsZero() && thinkingDurationMs < 0 {
				thinkingDurationMs = max(0, int(time.Since(thinkingStartedAt).Milliseconds()))
				out.send(Event{Type: "reasoning_done", Data: map[string]any{"duration_ms": thinkingDurationMs}})
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
		"provider":       a.cfg.Provider,
		"model":          a.cfg.Model,
		"context_window": a.cfg.ContextWindow,
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

func (a *Agent) executeToolCalls(ctx context.Context, toolCalls []message.Block, out *emitter) (message.Message, bool) {
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
		out.send(Event{Type: "tool_done", Data: data})
	}

	for _, toolCall := range toolCalls {
		input := maps.Clone(toolCall.Input)
		if input == nil {
			input = map[string]any{}
		}
		out.send(Event{Type: "tool_start", Data: map[string]any{
			"tool_call": map[string]any{
				"id":    toolCall.ID,
				"name":  toolCall.Name,
				"input": input,
			},
		}})

		toolCall.Input = input
		if ctx.Err() != nil || out.stopped {
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
	for _, spec := range a.cfg.Tools {
		if spec.Name == name {
			return spec, true
		}
	}
	return tools.Spec{}, false
}

func (a *Agent) runBeforeToolHooks(ctx context.Context, call tools.ToolCall) (tools.Result, bool) {
	for _, hook := range a.cfg.Hooks.BeforeTool {
		if result, handled := hook(ctx, call); handled {
			return result, true
		}
	}
	return tools.Result{}, false
}

func (a *Agent) runAfterToolHooks(ctx context.Context, call tools.ToolCall, result tools.Result) tools.Result {
	for _, hook := range a.cfg.Hooks.AfterTool {
		if next, replaced := hook(ctx, call, result); replaced {
			result = next
		}
	}
	return result
}

func (a *Agent) runToolSpec(ctx context.Context, toolCall message.Block, spec tools.Spec, out *emitter) tools.Result {
	var emitted []string
	var emit tools.OutputCallback
	if spec.StreamsOutput {
		emit = func(text string) {
			if ctx.Err() != nil {
				return
			}
			emitted = append(emitted, text)
			out.send(Event{Type: "tool_output", Data: map[string]any{
				"tool_use_id": toolCall.ID,
				"output":      text,
			}})
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
		CWD:   a.cfg.CWD,
		Emit:  emit,
	}
}

// compactIfNeeded triggers compaction when the latest turn crosses the
// threshold. Returns true to stop the agent loop (cancellation). Provider
// failures during compaction are swallowed — the next turn either retries or
// surfaces the error from phase 1.
func (a *Agent) compactIfNeeded(ctx context.Context, out *emitter, totalTokens int) bool {
	if !shouldCompact(totalTokens, a.cfg.ContextWindow, a.cfg.CompactThreshold) {
		return false
	}

	compactMessages := slices.Concat(
		applyCompactReplay(a.cfg.Messages, a.transcriptPath),
		[]message.Message{message.UserTextMessage(compactSummaryPrompt, nil)},
	)
	req := provider.Request{
		Provider:           a.cfg.Provider,
		Model:              a.cfg.Model,
		SessionID:          a.cfg.SessionID,
		Messages:           compactMessages,
		System:             a.cfg.System,
		MaxTokens:          min(a.cfg.MaxOutputTokens, 8192),
		Temperature:        a.cfg.Temperature,
		APIKey:             a.cfg.APIKey,
		APIBase:            a.cfg.APIBase,
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
		out.sendError("cancelled")
		return true
	}
	if summary == nil {
		return false
	}
	summaryText := message.FlattenText(*summary, false)
	if summaryText == "" {
		return false
	}
	compactEvent := buildCompactEvent(summaryText, a.cfg.Provider, a.cfg.Model, asInt(summary.Meta["total_tokens"]))
	if err := a.persist(compactEvent); err != nil {
		return false
	}
	a.cfg.Messages = append(a.cfg.Messages, compactEvent)
	out.send(Event{Type: "compact", Data: map[string]any{}})
	return false
}

func providerTools(specs []tools.Spec) []provider.ToolSpec {
	out := make([]provider.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, provider.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.InputSchema,
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
