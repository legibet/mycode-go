package provider

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/legibet/mycode-go/internal/message"
	"github.com/legibet/mycode-go/internal/tools"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	oparam "github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

type openAIResponsesAdapter struct {
	baseAdapter
}

func newOpenAIResponsesAdapter() Adapter {
	spec, _ := LookupSpec("openai")
	return openAIResponsesAdapter{baseAdapter: baseAdapter{spec: spec}}
}

func (a openAIResponsesAdapter) StreamTurn(ctx context.Context, req Request) <-chan StreamEvent {
	out := make(chan StreamEvent, 32)
	go func() {
		defer close(out)

		payload, err := a.buildPayload(req)
		if err != nil {
			out <- StreamEvent{Type: "provider_error", Err: err}
			return
		}
		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			out <- StreamEvent{Type: "provider_error", Err: err}
			return
		}

		opts := []option.RequestOption{
			option.WithAPIKey(req.APIKey),
			option.WithRequestTimeout(defaultRequestTimeout),
		}
		if baseURL := strings.TrimSpace(req.APIBase); baseURL != "" {
			opts = append(opts, option.WithBaseURL(strings.TrimRight(baseURL, "/")))
		}
		client := openai.NewClient(opts...)
		stream := client.Responses.NewStreaming(ctx, oparam.Override[responses.ResponseNewParams](json.RawMessage(bodyBytes)))
		defer func() { _ = stream.Close() }()

		var final *responses.Response
		doneItems := map[int]responses.ResponseOutputItemUnion{}

		for stream.Next() {
			if err := a.applyStreamEvent(stream.Current(), doneItems, &final, out); err != nil {
				out <- StreamEvent{Type: "provider_error", Err: err}
				return
			}
		}
		if err := stream.Err(); err != nil {
			// The SDK decodes a blank trailing SSE dispatch as empty JSON.
			if final == nil || !isBlankSSEDispatchError(err) {
				out <- StreamEvent{Type: "provider_error", Err: err}
				return
			}
		}

		if final == nil {
			out <- StreamEvent{Type: "provider_error", Err: errors.New("openai responses stream ended before response.completed")}
			return
		}

		items := make([]responses.ResponseOutputItemUnion, 0, len(doneItems))
		for _, idx := range slices.Sorted(maps.Keys(doneItems)) {
			items = append(items, doneItems[idx])
		}
		msg := a.convertResponse(*final, items)
		out <- StreamEvent{Type: "message_done", Msg: &msg}
	}()
	return out
}

func isBlankSSEDispatchError(err error) bool {
	var syntaxErr *json.SyntaxError
	return errors.As(err, &syntaxErr) && syntaxErr.Offset == 0
}

func (a openAIResponsesAdapter) applyStreamEvent(event responses.ResponseStreamEventUnion, doneItems map[int]responses.ResponseOutputItemUnion, final **responses.Response, out chan<- StreamEvent) error {
	switch event.Type {
	case "response.reasoning_text.delta":
		if event.Delta != "" {
			out <- StreamEvent{Type: "thinking_delta", Text: event.Delta}
		}
	case "response.output_text.delta":
		if event.Delta != "" {
			out <- StreamEvent{Type: "text_delta", Text: event.Delta}
		}
	case "response.output_item.done":
		doneItems[int(event.OutputIndex)] = event.Item
	case "response.completed":
		response := event.Response
		*final = &response
	case "error":
		return errors.New(strings.TrimSpace(event.Message))
	case "response.failed":
		msg := "response failed"
		if event.Response.Error.Message != "" {
			msg = event.Response.Error.Message
		}
		return errors.New(msg)
	}
	return nil
}

func (a openAIResponsesAdapter) buildPayload(req Request) (map[string]any, error) {
	prepared := prepareMessages(req, defaultProjectToolCallID)
	inputItems := make([]any, 0)
	for _, msg := range prepared {
		switch msg.Role {
		case "user":
			serialized, err := a.serializeUserMessage(msg)
			if err != nil {
				return nil, err
			}
			inputItems = append(inputItems, serialized...)
		case "assistant":
			if nativeItems := a.nativeOutputItems(msg); len(nativeItems) > 0 {
				inputItems = append(inputItems, nativeItems...)
				continue
			}
			inputItems = append(inputItems, a.serializeFallbackAssistantMessage(msg)...)
		}
	}

	payload := map[string]any{
		"model":             req.Model,
		"input":             inputItems,
		"store":             false,
		"include":           []string{"reasoning.encrypted_content"},
		"max_output_tokens": req.MaxTokens,
	}
	if req.System != "" {
		payload["instructions"] = req.System
	}
	if req.SessionID != "" {
		payload["prompt_cache_key"] = req.SessionID
	}
	if len(req.Tools) > 0 {
		toolsPayload := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			toolsPayload = append(toolsPayload, a.serializeTool(tool))
		}
		payload["tools"] = toolsPayload
		payload["tool_choice"] = "auto"
	}
	if req.ReasoningEffort != "" {
		payload["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
	}
	return payload, nil
}

func (a openAIResponsesAdapter) serializeUserMessage(msg message.Message) ([]any, error) {
	items := make([]any, 0)
	blocks := msg.Content
	messageBlocks := make([]message.Block, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" || block.Type == "image" || block.Type == "document" {
			messageBlocks = append(messageBlocks, block)
		}
	}
	content, err := a.serializeInputContent(messageBlocks)
	if err != nil {
		return nil, err
	}
	if len(content) > 0 {
		items = append(items, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": content,
		})
	}

	for _, block := range blocks {
		if block.Type != "tool_result" {
			continue
		}
		resultBlocks := ToolResultContentBlocks(block)
		hasImages := false
		for _, item := range resultBlocks {
			if item.Type == "image" {
				hasImages = true
				break
			}
		}
		output := any(block.Output)
		if hasImages {
			output, err = a.serializeInputContent(resultBlocks)
			if err != nil {
				return nil, err
			}
		}
		items = append(items, map[string]any{
			"type":    "function_call_output",
			"call_id": block.ToolUseID,
			"output":  output,
		})
	}
	return items, nil
}

func (a openAIResponsesAdapter) serializeInputContent(blocks []message.Block) ([]any, error) {
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			content = append(content, map[string]any{"type": "input_text", "text": block.Text})
		case "image":
			mimeType, data, err := loadImageBlockPayload(block)
			if err != nil {
				return nil, err
			}
			content = append(content, map[string]any{
				"type":      "input_image",
				"image_url": "data:" + mimeType + ";base64," + data,
			})
		case "document":
			mimeType, data, name, err := loadDocumentBlockPayload(block)
			if err != nil {
				return nil, err
			}
			content = append(content, map[string]any{
				"type":      "input_file",
				"filename":  cmp.Or(name, "document.pdf"),
				"file_data": "data:" + mimeType + ";base64," + data,
			})
		}
	}
	return content, nil
}

func (a openAIResponsesAdapter) nativeOutputItems(msg message.Message) []any {
	if msg.Meta["provider"] != a.Spec().ID {
		return nil
	}
	outputItems, _ := messageNativeMeta(msg)["output_items"].([]any)
	if len(outputItems) == 0 {
		return nil
	}
	replay := make([]any, 0, len(outputItems))
	for _, item := range outputItems {
		copied, ok := dumpJSON(item).(map[string]any)
		if !ok {
			continue
		}
		delete(copied, "status")
		if copied["type"] != "reasoning" {
			delete(copied, "id")
		}
		replay = append(replay, copied)
	}
	return replay
}

func (a openAIResponsesAdapter) serializeFallbackAssistantMessage(msg message.Message) []any {
	items := make([]any, 0)
	textParts := make([]string, 0)
	for _, block := range msg.Content {
		if block.Type == "text" && block.Text != "" {
			textParts = append(textParts, block.Text)
		}
	}
	if len(textParts) > 0 {
		items = append(items, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": strings.Join(textParts, "\n"),
			}},
		})
	}
	for _, block := range msg.Content {
		if block.Type != "tool_use" {
			continue
		}
		items = append(items, map[string]any{
			"type":      "function_call",
			"call_id":   block.ID,
			"name":      block.Name,
			"arguments": mustJSON(block.Input),
		})
	}
	return items
}

func (a openAIResponsesAdapter) serializeTool(tool map[string]any) map[string]any {
	parameters := dumpJSONMap(tool["input_schema"])
	properties, _ := parameters["properties"].(map[string]any)
	requiredList, _ := parameters["required"].([]any)
	required := map[string]struct{}{}
	for _, item := range requiredList {
		if name, ok := item.(string); ok {
			required[name] = struct{}{}
		}
	}
	if properties != nil {
		copied := dumpJSONMap(properties)
		names := slices.Sorted(maps.Keys(copied))
		for name, rawSchema := range copied {
			schema, ok := rawSchema.(map[string]any)
			if !ok {
				copied[name] = map[string]any{"anyOf": []any{rawSchema, map[string]any{"type": "null"}}}
				continue
			}
			if _, ok := required[name]; ok {
				continue
			}
			switch fieldType := schema["type"].(type) {
			case string:
				schema["type"] = []any{fieldType, "null"}
			case []any:
				if !slices.ContainsFunc(fieldType, func(item any) bool {
					text, ok := item.(string)
					return ok && text == "null"
				}) {
					schema["type"] = append(fieldType, "null")
				}
			default:
				copied[name] = map[string]any{"anyOf": []any{schema, map[string]any{"type": "null"}}}
				continue
			}
			if enumValues, ok := schema["enum"].([]any); ok {
				if !slices.Contains(enumValues, any(nil)) {
					schema["enum"] = append(enumValues, nil)
				}
			}
			copied[name] = schema
		}
		parameters["properties"] = copied
		requiredKeys := make([]any, len(names))
		for i, name := range names {
			requiredKeys[i] = name
		}
		parameters["required"] = requiredKeys
	}
	return map[string]any{
		"type":        "function",
		"name":        tool["name"],
		"description": tool["description"],
		"parameters":  parameters,
		"strict":      true,
	}
}

func (a openAIResponsesAdapter) convertResponse(response responses.Response, outputItems []responses.ResponseOutputItemUnion) message.Message {
	rawOutput := outputItems
	if len(rawOutput) == 0 {
		rawOutput = response.Output
	}
	blocks := make([]message.Block, 0, len(rawOutput))
	for _, item := range rawOutput {
		switch variant := item.AsAny().(type) {
		case responses.ResponseReasoningItem:
			textParts := make([]string, 0)
			for _, content := range variant.Content {
				if content.Text != "" {
					textParts = append(textParts, content.Text)
				}
			}
			if len(textParts) == 0 {
				for _, summary := range variant.Summary {
					if summary.Text != "" {
						textParts = append(textParts, summary.Text)
					}
				}
			}
			native := map[string]any{
				"item_id": variant.ID,
				"status":  string(variant.Status),
			}
			if summary := dumpJSON(variant.Summary); summary != nil {
				native["summary"] = summary
			}
			blocks = append(blocks, message.ThinkingBlock(strings.Join(textParts, ""), map[string]any{"native": native}))
		case responses.ResponseOutputMessage:
			for _, part := range variant.Content {
				if content, ok := part.AsAny().(responses.ResponseOutputText); ok {
					meta := map[string]any{}
					if annotations := dumpJSON(content.Annotations); annotations != nil {
						meta["native"] = map[string]any{"annotations": annotations}
					}
					blocks = append(blocks, message.TextBlock(content.Text, meta))
				}
			}
		case responses.ResponseFunctionToolCall:
			toolInput, err := tools.ParseToolArguments(variant.Arguments)
			native := map[string]any{
				"item_id": variant.ID,
				"status":  string(variant.Status),
			}
			if err != nil {
				native["raw_arguments"] = variant.Arguments
				toolInput = map[string]any{}
			}
			blocks = append(blocks, message.ToolUseBlock(variant.CallID, variant.Name, toolInput, map[string]any{"native": native}))
		}
	}

	nativeMeta := map[string]any{}
	if len(rawOutput) > 0 {
		stored := make([]any, 0, len(rawOutput))
		for _, item := range rawOutput {
			var dumped any
			if err := json.Unmarshal([]byte(item.RawJSON()), &dumped); err != nil {
				continue
			}
			stored = append(stored, dumped)
		}
		if len(stored) > 0 {
			nativeMeta["output_items"] = stored
		}
	}
	return message.AssistantMessage(
		blocks,
		a.Spec().ID,
		response.Model,
		response.ID,
		string(response.Status),
		mapTokenCount(dumpJSON(response.Usage), "total_tokens"),
		nativeMeta,
	)
}

func mustJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}
