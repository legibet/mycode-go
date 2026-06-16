package provider

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3/responses"
	"google.golang.org/genai"

	"github.com/legibet/mycode-go/message"
	"github.com/legibet/mycode-go/tools"
)

var (
	pdfBytes = []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")
	pngBytes = mustBase64Decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j1X8AAAAASUVORK5CYII=")
)

type payloadBuilder interface {
	buildPayload(Request) (map[string]any, error)
}

func mustPayload(t *testing.T, adapter payloadBuilder, req Request) map[string]any {
	t.Helper()
	payload, err := adapter.buildPayload(req)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mustAnthropicBlock(t *testing.T, adapter anthropicAdapter, block message.Block) map[string]any {
	t.Helper()
	payload, err := adapter.serializeBlock(block)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mustContents(t *testing.T, adapter googleAdapter, req Request) []*genai.Content {
	t.Helper()
	contents, err := adapter.buildContents(req)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestRepairMessagesForReplayDowngradesUnsupportedPDF(t *testing.T) {
	replay := RepairMessagesForReplay([]message.Message{
		message.BuildMessage("user", []message.Block{
			message.TextBlock("check this", nil),
			message.DocumentBlock(base64.StdEncoding.EncodeToString(pdfBytes), "application/pdf", `report <"draft">.pdf`, nil),
		}, nil),
	}, true, false)

	if len(replay) != 1 || len(replay[0].Content) != 2 {
		t.Fatalf("unexpected replay: %#v", replay)
	}
	if replay[0].Content[1].Type != "text" || replay[0].Content[1].Text != `<file name="report &lt;&quot;draft&quot;&gt;.pdf" media_type="application/pdf" kind="document">Current model does not support PDF input.</file>` {
		t.Fatalf("unexpected attachment downgrade: %#v", replay[0].Content[1])
	}
}

func TestAllBuiltInProvidersRegisterAdapters(t *testing.T) {
	for _, spec := range Specs() {
		adapter, ok := LookupAdapter(spec.ID)
		if !ok {
			t.Fatalf("missing adapter for provider %q", spec.ID)
		}
		if adapter.Spec().ID != spec.ID {
			t.Fatalf("adapter %q registered as %q", spec.ID, adapter.Spec().ID)
		}
	}
}

func TestAnthropicSerializeBlockSkipsToolUseCaller(t *testing.T) {
	adapter := newAnthropicAdapter("anthropic").(anthropicAdapter)
	block := message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, map[string]any{
		"native": map[string]any{"caller": map[string]any{"tool_id": "", "type": ""}},
	})
	payload := mustAnthropicBlock(t, adapter, block)
	if _, ok := payload["caller"]; ok {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestAnthropicConvertMessageKeepsStopSequenceAndServiceTier(t *testing.T) {
	adapter := newAnthropicAdapter("anthropic").(anthropicAdapter)
	raw := []byte(`{
		"id":"msg_123",
		"model":"claude-sonnet-4-6",
		"stop_reason":"tool_use",
		"stop_sequence":"stop-here",
		"usage":{"input_tokens":10,"output_tokens":5,"service_tier":"priority"},
		"content":[
			{"type":"tool_use","id":"call_1","name":"read","input":{"path":"x.py"}}
		]
	}`)
	var msg anthropic.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	converted := adapter.convertMessage(msg)
	native, _ := converted.Meta["native"].(map[string]any)
	if native["stop_sequence"] != "stop-here" || native["service_tier"] != "priority" {
		t.Fatalf("unexpected native meta: %#v", converted.Meta)
	}
}

func TestOpenAIResponsesSerializeToolMakesOptionalFieldsNullable(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	read := tools.Read
	readTool, err := adapter.serializeTool(ToolSpec{
		Name:        read.Name,
		Description: read.Description,
		InputSchema: read.InputSchema,
	})
	if err != nil {
		t.Fatal(err)
	}
	parameters, _ := readTool["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	offset, _ := properties["offset"].(map[string]any)
	limit, _ := properties["limit"].(map[string]any)
	if readTool["strict"] != true {
		t.Fatalf("unexpected tool: %#v", readTool)
	}
	if got := offset["type"]; !containsAnyString(got, "null") {
		t.Fatalf("offset should be nullable: %#v", readTool)
	}
	if got := limit["type"]; !containsAnyString(got, "null") {
		t.Fatalf("limit should be nullable: %#v", readTool)
	}
}

func TestOpenAIResponsesSerializeToolDefaultsMissingSchema(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	tool, err := adapter.serializeTool(ToolSpec{})
	if err != nil {
		t.Fatal(err)
	}
	parameters, _ := tool["parameters"].(map[string]any)
	if tool["name"] != "" || tool["description"] != "" {
		t.Fatalf("unexpected tool metadata: %#v", tool)
	}
	if parameters["type"] != "object" || parameters["additionalProperties"] != false {
		t.Fatalf("unexpected default schema: %#v", parameters)
	}
	if !reflect.DeepEqual(parameters["properties"], map[string]any{}) || !reflect.DeepEqual(parameters["required"], []any{}) {
		t.Fatalf("unexpected default schema: %#v", parameters)
	}
}

func TestOpenAIResponsesSerializesNestedStrictToolSchemas(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	payload := mustPayload(t, adapter, Request{
		Model: "gpt-5.4",
		Messages: []message.Message{
			message.UserTextMessage("hello", nil),
		},
		Tools: []ToolSpec{{
			Name:        "patch",
			Description: "Patch a file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string"},
					"limit": map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}, "default": nil},
					"mode":  map[string]any{"type": "string", "enum": []any{"fast", "safe"}, "default": "fast"},
					"fixed": map[string]any{"type": "string", "const": "stable", "default": "stable"},
					"edits": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/EditEntry"}},
				},
				"required": []any{"path", "edits"},
				"$defs": map[string]any{
					"EditEntry": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"oldText": map[string]any{"type": "string"},
							"newText": map[string]any{"type": "string"},
						},
						"required": []any{"oldText", "newText"},
					},
				},
			},
		}},
	})

	toolsPayload, _ := payload["tools"].([]any)
	serializedTool, _ := toolsPayload[0].(map[string]any)
	parameters, _ := serializedTool["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	if serializedTool["strict"] != true || parameters["additionalProperties"] != false {
		t.Fatalf("unexpected strict schema: %#v", serializedTool)
	}
	if !reflect.DeepEqual(parameters["required"], []any{"edits", "fixed", "limit", "mode", "path"}) {
		t.Fatalf("unexpected required fields: %#v", parameters["required"])
	}

	limit, _ := properties["limit"].(map[string]any)
	if _, ok := limit["default"]; ok {
		t.Fatalf("default should be removed: %#v", limit)
	}
	if !reflect.DeepEqual(limit["anyOf"], []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}) {
		t.Fatalf("unexpected nullable anyOf: %#v", limit)
	}

	mode, _ := properties["mode"].(map[string]any)
	if !reflect.DeepEqual(mode["type"], []any{"string", "null"}) || !reflect.DeepEqual(mode["enum"], []any{"fast", "safe", nil}) {
		t.Fatalf("unexpected nullable enum: %#v", mode)
	}
	fixed, _ := properties["fixed"].(map[string]any)
	if !reflect.DeepEqual(fixed["enum"], []any{"stable", nil}) {
		t.Fatalf("unexpected nullable const: %#v", fixed)
	}

	defs, _ := parameters["$defs"].(map[string]any)
	editEntry, _ := defs["EditEntry"].(map[string]any)
	if editEntry["additionalProperties"] != false || !reflect.DeepEqual(editEntry["required"], []any{"newText", "oldText"}) {
		t.Fatalf("unexpected nested object schema: %#v", editEntry)
	}
}

func TestOpenAIResponsesRejectsDynamicObjectToolSchemas(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	_, err := adapter.buildPayload(Request{
		Model: "gpt-5.4",
		Messages: []message.Message{
			message.UserTextMessage("hello", nil),
		},
		Tools: []ToolSpec{{
			Name:        "search",
			Description: "Search entries.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filters": map[string]any{
						"type":                 "object",
						"additionalProperties": map[string]any{"type": "string"},
					},
				},
				"required": []any{"filters"},
			},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "dynamic keys") {
		t.Fatalf("expected dynamic key schema error, got %v", err)
	}
}

func TestOpenAIResponsesBuildPayloadIncludesPromptCacheKey(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	payload := mustPayload(t, adapter, Request{
		Model:     "gpt-5.4",
		SessionID: "session_123",
		Messages: []message.Message{
			message.UserTextMessage("hello", nil),
		},
		System:    "You are helpful.",
		MaxTokens: 4096,
	})

	if payload["prompt_cache_key"] != "session_123" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	if payload["store"] != false {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	include, _ := payload["include"].([]string)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	if _, ok := payload["previous_response_id"]; ok {
		t.Fatalf("unexpected previous_response_id: %#v", payload)
	}
}

func TestOpenAIResponsesTextStreamIgnoresTrailingEmptyEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		defer func() { _ = r.Body.Close() }()
		_, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.output_text.delta\n"+
				"data: {\"type\":\"response.output_text.delta\",\"content_index\":0,\"delta\":\"OK\",\"item_id\":\"msg_1\",\"logprobs\":[],\"output_index\":1,\"sequence_number\":1}\n\n"+
				"event: response.output_item.done\n"+
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"annotations\":[],\"logprobs\":[],\"text\":\"OK\"}],\"phase\":\"final_answer\",\"role\":\"assistant\"},\"output_index\":1,\"sequence_number\":2}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.4-mini\",\"object\":\"response\",\"output\":[],\"status\":\"completed\"}}\n\n\n",
		)
	}))
	defer server.Close()

	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	stream := adapter.StreamTurn(t.Context(), Request{
		Model:     "gpt-5.4-mini",
		APIKey:    "sk-test",
		APIBase:   server.URL,
		MaxTokens: 64,
		Messages:  []message.Message{message.UserTextMessage("hi", nil)},
	})

	text := strings.Builder{}
	var final *message.Message
	for event := range stream {
		switch event.Type {
		case "provider_error":
			t.Fatalf("unexpected provider error: %v", event.Err)
		case "text_delta":
			text.WriteString(event.Text)
		case "message_done":
			final = event.Msg
		}
	}

	if text.String() != "OK" {
		t.Fatalf("unexpected text delta: %q", text.String())
	}
	if final == nil {
		t.Fatal("missing final message")
	}
	if len(final.Content) != 1 || final.Content[0].Type != "text" || final.Content[0].Text != "OK" {
		t.Fatalf("unexpected final message: %#v", final.Content)
	}
}

func TestOpenAIResponsesToolCallStreamIgnoresTrailingEmptyEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		_, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.output_item.done\n"+
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"x.py\\\"}\",\"status\":\"completed\"},\"output_index\":0,\"sequence_number\":1}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_2\",\"model\":\"gpt-5.4-mini\",\"object\":\"response\",\"output\":[],\"status\":\"completed\"}}\n\n\n",
		)
	}))
	defer server.Close()

	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	stream := adapter.StreamTurn(t.Context(), Request{
		Model:     "gpt-5.4-mini",
		APIKey:    "sk-test",
		APIBase:   server.URL,
		MaxTokens: 64,
		Messages:  []message.Message{message.UserTextMessage("use a tool", nil)},
	})

	var final *message.Message
	for event := range stream {
		if event.Type == "provider_error" {
			t.Fatalf("unexpected provider error: %v", event.Err)
		}
		if event.Type == "message_done" {
			final = event.Msg
		}
	}

	if final == nil {
		t.Fatal("missing final message")
	}
	if len(final.Content) != 1 {
		t.Fatalf("unexpected final blocks: %#v", final.Content)
	}
	block := final.Content[0]
	if block.Type != "tool_use" || block.ID != "call_1" || block.Name != "read" {
		t.Fatalf("unexpected tool block: %#v", block)
	}
	if path, _ := block.Input["path"].(string); path != "x.py" {
		t.Fatalf("unexpected tool input: %#v", block.Input)
	}
}

func TestOpenAIResponsesPreservesInvalidToolArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		_, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.output_item.done\n"+
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"read\",\"arguments\":\"{not json\",\"status\":\"completed\"},\"output_index\":0,\"sequence_number\":1}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_2\",\"model\":\"gpt-5.4-mini\",\"object\":\"response\",\"output\":[],\"status\":\"completed\"}}\n\n",
		)
	}))
	defer server.Close()

	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	stream := adapter.StreamTurn(t.Context(), Request{
		Model:     "gpt-5.4-mini",
		APIKey:    "sk-test",
		APIBase:   server.URL,
		MaxTokens: 64,
		Messages:  []message.Message{message.UserTextMessage("use a tool", nil)},
	})

	var final *message.Message
	for event := range stream {
		if event.Type == "provider_error" {
			t.Fatalf("unexpected provider error: %v", event.Err)
		}
		if event.Type == "message_done" {
			final = event.Msg
		}
	}

	if final == nil || len(final.Content) != 1 {
		t.Fatalf("unexpected final message: %#v", final)
	}
	block := final.Content[0]
	if block.Type != "tool_use" || block.ID != "call_1" || block.Name != "read" {
		t.Fatalf("unexpected tool block: %#v", block)
	}
	if len(block.Input) != 0 {
		t.Fatalf("expected empty tool input for invalid arguments, got %#v", block.Input)
	}
	native, _ := block.Meta["native"].(map[string]any)
	if native["raw_arguments"] != "{not json" {
		t.Fatalf("unexpected native metadata: %#v", block.Meta)
	}
}

func TestOpenAIResponsesReplaysNativeOutputItemsForToolResults(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	payload := mustPayload(t, adapter, Request{
		Model: "gpt-5.4",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
			}, "openai", "gpt-5.4", "", "", 0, map[string]any{
				"output_items": []any{
					map[string]any{
						"type":              "reasoning",
						"id":                "rs_1",
						"status":            "completed",
						"summary":           []any{},
						"encrypted_content": "enc_1",
					},
					map[string]any{
						"type":    "message",
						"id":      "msg_1",
						"role":    "assistant",
						"phase":   "commentary",
						"status":  "completed",
						"content": []any{map[string]any{"type": "output_text", "text": "Checking the file."}},
					},
					map[string]any{
						"type":      "function_call",
						"id":        "fc_1",
						"call_id":   "call_1",
						"name":      "read",
						"arguments": `{"path":"x.py"}`,
						"status":    "completed",
					},
				},
			}),
			message.BuildMessage("user", []message.Block{
				message.ToolResultBlock("call_1", "file contents", nil, false, nil, nil),
			}, nil),
		},
		MaxTokens: 4096,
	})

	input := payload["input"].([]any)
	if !reflect.DeepEqual(inputTypes(input), []string{"reasoning", "message", "function_call", "function_call_output"}) {
		t.Fatalf("unexpected input item types: %#v", input)
	}
	reasoning := input[0].(map[string]any)
	messageItem := input[1].(map[string]any)
	call := input[2].(map[string]any)
	output := input[3].(map[string]any)
	if reasoning["encrypted_content"] != "enc_1" {
		t.Fatalf("missing reasoning replay data: %#v", reasoning)
	}
	if messageItem["phase"] != "commentary" || firstContentText(messageItem["content"]) != "Checking the file." {
		t.Fatalf("unexpected assistant replay item: %#v", messageItem)
	}
	if call["call_id"] != "call_1" || call["arguments"] != `{"path":"x.py"}` {
		t.Fatalf("unexpected function call replay: %#v", call)
	}
	if output["call_id"] != "call_1" || output["output"] != "file contents" {
		t.Fatalf("unexpected tool output replay: %#v", output)
	}
}

func TestOpenAIResponsesFallbackReplaySkipsReasoningBlocks(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	payload := mustPayload(t, adapter, Request{
		Model: "gpt-5.4",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ThinkingBlock("Need the tool first.", nil),
				message.TextBlock("I will inspect the file.", nil),
				message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
			}, "openai", "gpt-5.4", "", "", 0, nil),
		},
		MaxTokens: 4096,
	})

	input := payload["input"].([]any)
	if !reflect.DeepEqual(inputTypes(input), []string{"message", "function_call", "function_call_output"}) {
		t.Fatalf("unexpected input item types: %#v", input)
	}
	messageItem := input[0].(map[string]any)
	if firstContentText(messageItem["content"]) != "I will inspect the file." {
		t.Fatalf("unexpected assistant text replay: %#v", messageItem)
	}
	call := input[1].(map[string]any)
	if call["call_id"] != "call_1" || call["arguments"] != `{"path":"x.py"}` {
		t.Fatalf("unexpected function call replay: %#v", call)
	}
	output := input[2].(map[string]any)
	if output["call_id"] != "call_1" || output["output"] != "error: tool call was interrupted" {
		t.Fatalf("unexpected interrupted tool result: %#v", output)
	}
}

func TestUserPDFInputSerialization(t *testing.T) {
	pdfData := base64.StdEncoding.EncodeToString(pdfBytes)
	request := Request{
		Model:              "gpt-5.4",
		SupportsImageInput: true,
		SupportsPDFInput:   true,
		Messages: []message.Message{
			message.BuildMessage("user", []message.Block{
				message.TextBlock("summarize", nil),
				message.DocumentBlock(pdfData, "application/pdf", "report.pdf", nil),
			}, nil),
		},
	}

	responsesAdapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	responsesPayload := mustPayload(t, responsesAdapter, request)
	input := responsesPayload["input"].([]any)
	content := input[0].(map[string]any)["content"].([]any)
	fileInput, _ := content[1].(map[string]any)
	if fileInput["type"] != "input_file" || fileInput["filename"] != "report.pdf" || fileInput["file_data"] != "data:application/pdf;base64,"+pdfData {
		t.Fatalf("unexpected responses content: %#v", content)
	}

	chatAdapter := newOpenAIChatAdapter("openai_chat").(openAIChatAdapter)
	chatPayload := mustPayload(t, chatAdapter, request)
	chatMessages := chatPayload["messages"].([]any)
	chatContent := chatMessages[0].(map[string]any)["content"].([]any)
	filePart := chatContent[1].(map[string]any)
	if filePart["type"] != "file" {
		t.Fatalf("unexpected chat content: %#v", chatContent)
	}

	anthropicAdapter := newAnthropicAdapter("anthropic").(anthropicAdapter)
	anthropicPayload := mustPayload(t, anthropicAdapter, Request{
		Model:              "claude-sonnet-4-6",
		SupportsImageInput: true,
		SupportsPDFInput:   true,
		Messages:           request.Messages,
	})
	anthropicMessages := anthropicPayload["messages"].([]map[string]any)
	anthropicContent := anthropicMessages[0]["content"].([]map[string]any)
	if anthropicContent[1]["type"] != "document" {
		t.Fatalf("unexpected anthropic content: %#v", anthropicContent)
	}

	googleAdapter := newGoogleAdapter().(googleAdapter)
	googleContent := mustContents(t, googleAdapter, Request{
		Model:              "gemini-3-flash-preview",
		SupportsImageInput: true,
		SupportsPDFInput:   true,
		Messages:           request.Messages,
	})
	if googleContent[0].Parts[1].InlineData == nil || googleContent[0].Parts[1].InlineData.MIMEType != "application/pdf" {
		t.Fatalf("unexpected google content: %#v", googleContent)
	}
}

func TestRepairMessagesForReplayDropsDuplicateAndOrphanToolResults(t *testing.T) {
	replay := RepairMessagesForReplay([]message.Message{
		message.AssistantMessage([]message.Block{
			message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
		}, "", "", "", "", 0, nil),
		message.BuildMessage("user", []message.Block{
			message.ToolResultBlock("call_1", "first", nil, false, nil, nil),
			message.ToolResultBlock("call_1", "duplicate", nil, false, nil, nil),
			message.ToolResultBlock("call_2", "orphan", nil, false, nil, nil),
		}, nil),
		message.AssistantMessage([]message.Block{
			message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
		}, "", "", "", "", 0, nil),
	}, true, true)

	if len(replay) != 2 || replay[0].Role != "assistant" || replay[1].Role != "user" {
		t.Fatalf("unexpected replay roles: %#v", replay)
	}
	if replay[0].Content[0].Type != "tool_use" || replay[0].Content[0].ID != "call_1" {
		t.Fatalf("unexpected tool use replay: %#v", replay[0])
	}
	if len(replay[1].Content) != 1 || replay[1].Content[0].ToolUseID != "call_1" || replay[1].Content[0].Output != "first" {
		t.Fatalf("unexpected tool result replay: %#v", replay[1])
	}
}

func TestRepairMessagesForReplayKeepsPlaceholderUserTurn(t *testing.T) {
	replay := RepairMessagesForReplay([]message.Message{
		message.AssistantMessage([]message.Block{
			message.TextBlock("first", nil),
		}, "", "", "", "", 0, nil),
		message.BuildMessage("user", []message.Block{
			message.ToolResultBlock("missing", "orphan", nil, false, nil, nil),
		}, nil),
		message.AssistantMessage([]message.Block{
			message.TextBlock("second", nil),
		}, "", "", "", "", 0, nil),
	}, true, true)

	if len(replay) != 3 || replay[0].Content[0].Text != "first" || replay[2].Content[0].Text != "second" {
		t.Fatalf("unexpected replay: %#v", replay)
	}
	if replay[1].Role != "user" || replay[1].Content[0].Text != "[User turn omitted during replay]" || replay[1].Meta["synthetic"] != true {
		t.Fatalf("unexpected synthetic placeholder: %#v", replay[1])
	}
}

func TestRepairMessagesForReplayFiltersImagesWhenDisabled(t *testing.T) {
	replay := RepairMessagesForReplay([]message.Message{
		message.AssistantMessage([]message.Block{
			message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.png"}, nil),
		}, "", "", "", "", 0, nil),
		message.BuildMessage("user", []message.Block{
			message.TextBlock("describe this", nil),
			message.ImageBlock("abc", "image/png", `logo"<v2>.png`, nil),
			message.ToolResultBlock("call_1", "Read image file [image/png]", nil, false, []message.Block{
				message.TextBlock("Read image file [image/png]", nil),
				message.ImageBlock("abc", "image/png", "", nil),
			}, nil),
		}, nil),
	}, false, true)

	if len(replay) != 2 || len(replay[1].Content) != 3 {
		t.Fatalf("unexpected replay: %#v", replay)
	}
	notice := replay[1].Content[1]
	if notice.Type != "text" || !strings.Contains(notice.Text, `logo&quot;&lt;v2&gt;.png`) || notice.Meta["attachment"] != true {
		t.Fatalf("unexpected image downgrade notice: %#v", notice)
	}
	toolResult := replay[1].Content[2]
	if len(toolResult.Content) != 1 || toolResult.Content[0].Type != "text" {
		t.Fatalf("unexpected filtered tool result content: %#v", toolResult)
	}
}

func TestOpenAIResponsesFallsBackToFullReplayForCrossProviderHistory(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	payload := mustPayload(t, adapter, Request{
		Model: "gpt-5.4",
		Messages: []message.Message{
			message.UserTextMessage("double 21", nil),
			message.AssistantMessage([]message.Block{
				message.ThinkingBlock("Need the tool first.", nil),
				message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
			}, "anthropic", "claude-sonnet-4-6", "", "", 0, nil),
			message.BuildMessage("user", []message.Block{
				message.ToolResultBlock("call_1", "42", nil, false, nil, nil),
			}, nil),
		},
		MaxTokens: 4096,
	})

	input := payload["input"].([]any)
	if !reflect.DeepEqual(inputTypes(input), []string{"message", "function_call", "function_call_output"}) {
		t.Fatalf("unexpected replay input types: %#v", input)
	}
	user := input[0].(map[string]any)
	userContent := user["content"].([]any)
	if user["role"] != "user" || userContent[0].(map[string]any)["text"] != "double 21" {
		t.Fatalf("unexpected user replay: %#v", user)
	}
	call := input[1].(map[string]any)
	result := input[2].(map[string]any)
	if call["call_id"] != "call_1" || call["arguments"] != `{"path":"x.py"}` || result["output"] != "42" {
		t.Fatalf("unexpected tool replay: %#v", input)
	}
}

func TestOpenAIResponsesSerializesToolResultImages(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	payload := mustPayload(t, adapter, Request{
		Model:              "gpt-5.4",
		SupportsImageInput: true,
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.png"}, nil),
			}, "", "", "", "", 0, nil),
			message.BuildMessage("user", []message.Block{
				message.ToolResultBlock("call_1", "Read image file [image/png]", nil, false, []message.Block{
					message.TextBlock("Read image file [image/png]", nil),
					message.ImageBlock(base64.StdEncoding.EncodeToString(pngBytes), "image/png", "tiny.png", nil),
				}, nil),
			}, nil),
		},
		MaxTokens: 4096,
	})

	input := payload["input"].([]any)
	first := input[0].(map[string]any)
	second := input[1].(map[string]any)
	if first["type"] != "function_call" || second["type"] != "function_call_output" {
		t.Fatalf("unexpected input: %#v", input)
	}
	output, _ := second["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("unexpected output: %#v", second["output"])
	}
	text, _ := output[0].(map[string]any)
	if text["type"] != "input_text" || text["text"] != "Read image file [image/png]" {
		t.Fatalf("unexpected output text: %#v", output)
	}
	image, _ := output[1].(map[string]any)
	if image["type"] != "input_image" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestOpenAIResponsesConvertsFinalResponseBlocks(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	response := mustUnmarshalResponse(t, `{
		"id":"resp_123",
		"model":"gpt-5.4",
		"status":"completed",
		"usage":{"input_tokens":10,"output_tokens":5},
		"output":[
			{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"text":"think"}]},
			{"type":"message","content":[{"type":"output_text","text":"answer","annotations":[]}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\": \"x.py\"}","status":"completed"}
		]
	}`)

	msg := adapter.convertResponse(response, nil)
	if msg.Role != "assistant" || len(msg.Content) != 3 {
		t.Fatalf("unexpected message: %#v", msg)
	}
	if msg.Content[0].Type != "thinking" || msg.Content[0].Text != "think" {
		t.Fatalf("unexpected message: %#v", msg)
	}
	nativeBlockMeta, _ := msg.Content[0].Meta["native"].(map[string]any)
	summary, _ := nativeBlockMeta["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("unexpected thinking metadata: %#v", msg.Content[0].Meta)
	}
	firstSummary, _ := summary[0].(map[string]any)
	if nativeBlockMeta["item_id"] != "rs_1" || nativeBlockMeta["status"] != "completed" || firstSummary["text"] != "think" {
		t.Fatalf("unexpected thinking metadata: %#v", msg.Content[0].Meta)
	}
	if msg.Content[1].Type != "text" || msg.Content[1].Text != "answer" {
		t.Fatalf("unexpected text block: %#v", msg.Content[1])
	}
	if msg.Content[2].Type != "tool_use" || msg.Content[2].ID != "call_1" || !reflect.DeepEqual(msg.Content[2].Input, map[string]any{"path": "x.py"}) {
		t.Fatalf("unexpected tool block: %#v", msg.Content[2])
	}
	native, _ := msg.Meta["native"].(map[string]any)
	outputItems, _ := native["output_items"].([]any)
	if len(outputItems) != 3 {
		t.Fatalf("unexpected native meta: %#v", msg.Meta)
	}
	first, _ := outputItems[0].(map[string]any)
	if _, ok := first["acknowledged_safety_checks"]; ok {
		t.Fatalf("unexpected union junk in stored output item: %#v", first)
	}
	replayPayload := mustPayload(t, adapter, Request{
		Model:     "gpt-5.4",
		Messages:  []message.Message{msg},
		MaxTokens: 4096,
	})
	replayInput := replayPayload["input"].([]any)
	replayFirst, _ := replayInput[0].(map[string]any)
	if _, ok := replayFirst["acknowledged_safety_checks"]; ok {
		t.Fatalf("unexpected replay item: %#v", replayFirst)
	}
}

func TestOpenAIResponsesUsesStreamOutputItemsWhenFinalOutputIsEmpty(t *testing.T) {
	adapter := newOpenAIResponsesAdapter().(openAIResponsesAdapter)
	response := mustUnmarshalResponse(t, `{
		"id":"resp_123",
		"model":"gpt-5.4",
		"status":"completed",
		"usage":{"input_tokens":10,"output_tokens":5},
		"output":[]
	}`)
	outputItems := mustUnmarshalOutputItems(t, `[
		{"type":"message","content":[{"type":"output_text","text":"hello world","annotations":[]}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{\"path\":\"pyproject.toml\"}","status":"completed"}
	]`)

	msg := adapter.convertResponse(response, outputItems)
	if len(msg.Content) != 2 || msg.Content[0].Type != "text" || msg.Content[0].Text != "hello world" {
		t.Fatalf("unexpected message: %#v", msg)
	}
	toolUse := msg.Content[1]
	native, _ := toolUse.Meta["native"].(map[string]any)
	if toolUse.Type != "tool_use" || toolUse.ID != "call_1" || toolUse.Input["path"] != "pyproject.toml" || native["item_id"] != "fc_1" {
		t.Fatalf("unexpected tool use block: %#v", toolUse)
	}
}

func TestOpenAIChatExtractsReasoningFromKnownExtraFields(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		text string
		meta map[string]any
	}{
		{
			name: "root reasoning_content",
			raw:  `{"reasoning_content":"step zero"}`,
			text: "step zero",
			meta: map[string]any{"reasoning_field": "reasoning_content"},
		},
		{
			name: "model_extra reasoning_content",
			raw:  `{"model_extra":{"reasoning_content":"step one"}}`,
			text: "step one",
			meta: map[string]any{"reasoning_field": "reasoning_content"},
		},
		{
			name: "model_extra reasoning alias",
			raw:  `{"model_extra":{"reasoning":"step alias"}}`,
			text: "step alias",
			meta: map[string]any{"reasoning_field": "reasoning"},
		},
		{
			name: "empty reasoning_content marker",
			raw:  `{"model_extra":{"reasoning_content":null}}`,
			text: "",
			meta: map[string]any{"reasoning_field": "reasoning_content"},
		},
		{
			name: "model_extra reasoning_details",
			raw:  `{"model_extra":{"reasoning_details":[{"type":"reasoning.text","text":"step "},{"type":"reasoning.text","text":"two"}]}}`,
			text: "step two",
			meta: map[string]any{
				"reasoning_field": "reasoning_details",
				"reasoning_details": []any{
					map[string]any{"type": "reasoning.text", "text": "step "},
					map[string]any{"type": "reasoning.text", "text": "two"},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, meta := extractChatReasoningDelta(tc.raw)
			if text != tc.text || !reflect.DeepEqual(meta, tc.meta) {
				t.Fatalf("unexpected reasoning delta: %q %#v", text, meta)
			}
		})
	}
}

func TestRepairMessagesPreservesEmptyNativeReasoningBlocks(t *testing.T) {
	messages := []message.Message{
		message.AssistantMessage([]message.Block{
			message.ThinkingBlock("", map[string]any{"native": map[string]any{"reasoning_field": "reasoning_content"}}),
			message.TextBlock("done", nil),
		}, "", "", "", "", 0, nil),
		message.UserTextMessage("next question", nil),
	}

	repaired := RepairMessagesForReplay(messages, true, true)
	if len(repaired) == 0 || len(repaired[0].Content) == 0 || repaired[0].Content[0].Type != "thinking" {
		t.Fatalf("unexpected repaired messages: %#v", repaired)
	}
}

func TestOpenAIChatReplaysNativeReasoningField(t *testing.T) {
	adapter := newOpenAIChatAdapter("openai_chat").(openAIChatAdapter)
	payload := mustPayload(t, adapter, Request{
		Model: "test-model",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ThinkingBlock("", map[string]any{"native": map[string]any{"reasoning_field": "reasoning_content"}}),
				message.TextBlock("done", nil),
			}, "", "", "", "", 0, nil),
			message.UserTextMessage("next question", nil),
		},
		MaxTokens: 2048,
	})

	first, _ := payload["messages"].([]any)[0].(map[string]any)
	if value, ok := first["reasoning_content"]; !ok || value != nil {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestOpenAIChatReplaysReasoningByDefault(t *testing.T) {
	adapter := newOpenAIChatAdapter("openai_chat").(openAIChatAdapter)
	payload := mustPayload(t, adapter, Request{
		Model: "test-model",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ThinkingBlock("think", nil),
				message.TextBlock("answer", nil),
			}, "", "", "", "", 0, nil),
		},
		MaxTokens: 2048,
	})

	messages := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if first["reasoning_content"] != "think" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestDeepSeekReplaysReasoningAcrossTurns(t *testing.T) {
	adapter := newOpenAIChatAdapter("deepseek").(openAIChatAdapter)

	withToolCall := mustPayload(t, adapter, Request{
		Model: "test-model",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ThinkingBlock("think", map[string]any{"native": map[string]any{"reasoning_field": "reasoning_content"}}),
				message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
			}, "", "", "", "", 0, nil),
			message.BuildMessage("user", []message.Block{
				message.ToolResultBlock("call_1", "done", nil, false, nil, nil),
			}, nil),
		},
		MaxTokens: 2048,
	})
	first, _ := withToolCall["messages"].([]any)[0].(map[string]any)
	if first["reasoning_content"] != "think" {
		t.Fatalf("unexpected payload: %#v", withToolCall)
	}

	withNextUserTurn := mustPayload(t, adapter, Request{
		Model: "test-model",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ThinkingBlock("think", map[string]any{"native": map[string]any{"reasoning_field": "reasoning_content"}}),
				message.TextBlock("done", nil),
			}, "", "", "", "", 0, nil),
			message.UserTextMessage("next question", nil),
		},
		MaxTokens: 2048,
	})
	first, _ = withNextUserTurn["messages"].([]any)[0].(map[string]any)
	if first["reasoning_content"] != "think" {
		t.Fatalf("unexpected payload: %#v", withNextUserTurn)
	}
}

func TestAnthropicPrepareMessagesNormalizesToolIDs(t *testing.T) {
	adapter := newAnthropicAdapter("anthropic").(anthropicAdapter)
	prepared := prepareMessages(Request{
		Model: "claude-sonnet-4-6",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ToolUseBlock("a/b", "read", map[string]any{"path": "x.py"}, nil),
				message.ToolUseBlock("a|b", "write", map[string]any{"path": "y.py"}, nil),
			}, "anthropic", "claude-sonnet-4-6", "", "", 0, nil),
			message.BuildMessage("user", []message.Block{
				message.ToolResultBlock("a/b", "done a", nil, false, nil, nil),
				message.ToolResultBlock("a|b", "done b", nil, false, nil, nil),
			}, nil),
		},
	}, adapter.projectToolCallID)

	firstID := prepared[0].Content[0].ID
	secondID := prepared[0].Content[1].ID
	if firstID == secondID || len(firstID) == 0 || len(secondID) == 0 {
		t.Fatalf("unexpected prepared messages: %#v", prepared)
	}
	if prepared[1].Content[0].ToolUseID != firstID || prepared[1].Content[1].ToolUseID != secondID {
		t.Fatalf("unexpected prepared messages: %#v", prepared)
	}
}

func TestAnthropicBuildPayloadAddsCacheControlToLatestUserBlock(t *testing.T) {
	adapter := newAnthropicAdapter("anthropic").(anthropicAdapter)
	payload := mustPayload(t, adapter, Request{
		Model:     "test-model",
		MaxTokens: 4096,
		System:    "You are helpful.",
		Messages: []message.Message{
			message.UserTextMessage("first user message", nil),
			message.AssistantMessage([]message.Block{
				message.TextBlock("assistant reply", nil),
			}, "", "", "", "", 0, nil),
			message.AssistantMessage([]message.Block{
				message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, nil),
			}, "", "", "", "", 0, nil),
			message.BuildMessage("user", []message.Block{
				message.TextBlock("latest user message", nil),
				message.ToolResultBlock("call_1", "tool output", nil, false, nil, nil),
			}, nil),
		},
	})

	systemBlocks, _ := payload["system"].([]map[string]any)
	if len(systemBlocks) != 1 || cacheControlType(systemBlocks[0]["cache_control"]) != "ephemeral" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	messages := payload["messages"].([]map[string]any)
	firstContent := messages[0]["content"].([]map[string]any)
	if _, ok := firstContent[0]["cache_control"]; ok {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	lastContent := messages[3]["content"].([]map[string]any)
	if cacheControlType(lastContent[1]["cache_control"]) != "ephemeral" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestOpenAIChatAssistantReplayIncludesEmptyContent(t *testing.T) {
	adapter := newOpenAIChatAdapter("openai_chat").(openAIChatAdapter)
	payload := mustPayload(t, adapter, Request{
		Model: "gpt-5.4",
		Messages: []message.Message{
			message.AssistantMessage([]message.Block{
				message.ToolUseBlock("call_1", "read", map[string]any{"path": "README.md"}, nil),
			}, "openai_chat", "gpt-5.4", "", "", 0, nil),
		},
		MaxTokens: 4096,
	})
	messages := payload["messages"].([]any)
	assistant := messages[0].(map[string]any)
	if content, ok := assistant["content"]; !ok || content != "" {
		t.Fatalf("unexpected assistant payload: %#v", assistant)
	}
}

func TestGoogleBuildConfigUsesSupportedToolSettings(t *testing.T) {
	adapter := newGoogleAdapter().(googleAdapter)
	config := adapter.buildConfig(Request{
		Model:     "gemini-3-flash-preview",
		System:    "You are helpful.",
		MaxTokens: 2048,
		Tools: []ToolSpec{
			{
				Name:        "read",
				Description: "Read a file.",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
		},
	})

	if len(config.Tools) != 1 || len(config.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("unexpected config: %#v", config)
	}
	if config.Tools[0].FunctionDeclarations[0].Name != "read" {
		t.Fatalf("unexpected config: %#v", config)
	}
	if config.ToolConfig != nil {
		t.Fatalf("unexpected tool config: %#v", config)
	}
}

func TestGoogleHTTPOptionsDoNotUseSDKTimeout(t *testing.T) {
	options := googleHTTPOptions("")
	if options.APIVersion != "v1beta" {
		t.Fatalf("unexpected options: %#v", options)
	}
	if options.Timeout != nil {
		t.Fatalf("unexpected timeout: %#v", options)
	}
}

func TestGoogleBuildConfigOmitsUnsupportedExtrasProvider(t *testing.T) {
	adapter := newGoogleAdapter().(googleAdapter)
	config := adapter.buildConfig(Request{
		Model:     "gemini-3-flash-preview",
		System:    "You are helpful.",
		MaxTokens: 2048,
		Tools: []ToolSpec{
			{Name: "read", Description: "Read a file.", InputSchema: map[string]any{"type": "object"}},
		},
		ReasoningEffort: "none",
	})
	if config.HTTPOptions == nil {
		t.Fatalf("missing http options: %#v", config)
	}
	if config.HTTPOptions.ExtrasRequestProvider != nil {
		t.Fatalf("unexpected extras provider: %#v", config)
	}
}

func TestGoogleNativePartRestoresSDKShape(t *testing.T) {
	block := message.ToolUseBlock("call_1", "read", map[string]any{"path": "x.py"}, map[string]any{
		"native": map[string]any{
			"part": map[string]any{
				"function_call":     map[string]any{"id": "call_1", "name": "read", "args": map[string]any{"path": "x.py"}},
				"thought_signature": "c2ln",
			},
		},
	})

	part := googleNativePart(block)
	if part == nil || part.FunctionCall == nil {
		t.Fatalf("unexpected part: %#v", part)
	}
	if part.FunctionCall.ID != "call_1" || part.FunctionCall.Name != "read" {
		t.Fatalf("unexpected part: %#v", part)
	}
	if string(part.ThoughtSignature) != "sig" {
		t.Fatalf("unexpected part: %#v", part)
	}
	if !reflect.DeepEqual(part.FunctionCall.Args, map[string]any{"path": "x.py"}) {
		t.Fatalf("unexpected part: %#v", part)
	}
}

func TestGoogleHTTPOptionsDropsVersionWhenBaseIncludesAPIVersion(t *testing.T) {
	cases := []string{
		"https://example.test/v1",
		"https://example.test/v1beta",
	}
	for _, baseURL := range cases {
		t.Run(baseURL, func(t *testing.T) {
			options := googleHTTPOptions(baseURL)
			if options.APIVersion != "" {
				t.Fatalf("unexpected options: %#v", options)
			}
			if options.BaseURL != baseURL {
				t.Fatalf("unexpected options: %#v", options)
			}
		})
	}
}

func TestGoogleStreamingPartsMergeIntoFinalBlocks(t *testing.T) {
	adapter := newGoogleAdapter().(googleAdapter)
	var blocks []message.Block

	events := adapter.consumePart(&blocks, &genai.Part{Text: "step ", Thought: true})
	if len(events) != 1 || events[0].Type != "thinking_delta" || events[0].Text != "step " {
		t.Fatalf("unexpected events: %#v", events)
	}

	events = adapter.consumePart(&blocks, &genai.Part{Text: "one", Thought: true, ThoughtSignature: []byte("sig")})
	if len(events) != 1 || events[0].Type != "thinking_delta" || events[0].Text != "one" {
		t.Fatalf("unexpected events: %#v", events)
	}

	part := genai.NewPartFromFunctionCall("read", map[string]any{"path": "x.py"})
	part.FunctionCall.ID = "call_1"
	part.ThoughtSignature = []byte("sig")
	events = adapter.consumePart(&blocks, part)
	if len(events) != 0 {
		t.Fatalf("unexpected events: %#v", events)
	}
	if len(blocks) != 2 {
		t.Fatalf("unexpected blocks: %#v", blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].Text != "step one" || nativePart(blocks[0])["thought_signature"] != "c2ln" {
		t.Fatalf("unexpected thinking block: %#v", blocks[0])
	}
	functionCall, _ := nativePart(blocks[1])["function_call"].(map[string]any)
	if blocks[1].Type != "tool_use" || blocks[1].ID != "call_1" || functionCall["name"] != "read" || nativePart(blocks[1])["thought_signature"] != "c2ln" {
		t.Fatalf("unexpected tool block: %#v", blocks[1])
	}
}

func TestGoogleKeepsSignatureOnlyStreamChunk(t *testing.T) {
	adapter := newGoogleAdapter().(googleAdapter)
	blocks := []message.Block{}

	events := adapter.consumePart(&blocks, &genai.Part{Text: "", ThoughtSignature: []byte("sig")})
	if len(events) != 0 {
		t.Fatalf("unexpected events: %#v", events)
	}
	if len(blocks) != 1 || blocks[0].Type != "text" || nativePart(blocks[0])["thought_signature"] != "c2ln" {
		t.Fatalf("unexpected blocks: %#v", blocks)
	}
}

func containsAnyString(value any, target string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if text, ok := item.(string); ok && text == target {
			return true
		}
	}
	return false
}

func inputTypes(items []any) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		entry, _ := item.(map[string]any)
		kind, _ := entry["type"].(string)
		out = append(out, kind)
	}
	return out
}

func firstContentText(value any) string {
	switch content := value.(type) {
	case []any:
		if len(content) == 0 {
			return ""
		}
		entry, _ := content[0].(map[string]any)
		text, _ := entry["text"].(string)
		return text
	case []map[string]any:
		if len(content) == 0 {
			return ""
		}
		text, _ := content[0]["text"].(string)
		return text
	default:
		return ""
	}
}

func cacheControlType(value any) string {
	entry, _ := value.(map[string]any)
	text, _ := entry["type"].(string)
	return text
}

func nativePart(block message.Block) map[string]any {
	native, _ := block.Meta["native"].(map[string]any)
	part, _ := native["part"].(map[string]any)
	return part
}

func mustBase64Decode(value string) []byte {
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return data
}

func mustUnmarshalResponse(t *testing.T, raw string) responses.Response {
	t.Helper()
	var response responses.Response
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func mustUnmarshalOutputItems(t *testing.T, raw string) []responses.ResponseOutputItemUnion {
	t.Helper()
	var items []responses.ResponseOutputItemUnion
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatal(err)
	}
	return items
}

func TestInferProviderFromModel(t *testing.T) {
	cases := map[string]string{
		"claude-opus-4-8":         "anthropic",
		"anthropic/claude-opus-4": "anthropic",
		"deepseek-v4-pro":         "deepseek",
		"gemini-3.5-flash":        "google",
		"glm-5.2":                 "zai",
		"gpt-5.5":                 "openai",
		"o3":                      "openai",
		"kimi-k2.7-code":          "moonshotai",
		"MiniMax-M3":              "minimax",
	}
	for model, want := range cases {
		got, ok := InferProviderFromModel(model)
		if !ok || got != want {
			t.Fatalf("InferProviderFromModel(%q) = %q, %v; want %q, true", model, got, ok, want)
		}
	}

	for _, model := range []string{"", "mystery-model", "llama-3"} {
		if got, ok := InferProviderFromModel(model); ok {
			t.Fatalf("InferProviderFromModel(%q) = %q, true; want \"\", false", model, got)
		}
	}
}
