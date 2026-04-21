package message

import (
	"maps"
	"slices"
	"strings"
)

// Block is the canonical content block persisted in sessions and used in API responses.
type Block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Data      string         `json:"data,omitempty"`
	MIMEType  string         `json:"mime_type,omitempty"`
	Name      string         `json:"name,omitempty"`
	ID        string         `json:"id,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Output    string         `json:"output,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	IsError   *bool          `json:"is_error,omitempty"`
	Content   []Block        `json:"content,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// Message is the single runtime and persistence message format.
type Message struct {
	Role    string         `json:"role"`
	Content []Block        `json:"content,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// Bool returns a stable pointer for JSON optional booleans.
func Bool(v bool) *bool {
	return &v
}

// TextBlock returns a plain text block.
func TextBlock(text string, meta map[string]any) Block {
	b := Block{Type: "text", Text: text}
	if len(meta) > 0 {
		b.Meta = maps.Clone(meta)
	}
	return b
}

// ThinkingBlock returns a reasoning block.
func ThinkingBlock(text string, meta map[string]any) Block {
	b := Block{Type: "thinking", Text: text}
	if len(meta) > 0 {
		b.Meta = maps.Clone(meta)
	}
	return b
}

// ImageBlock returns an image block.
func ImageBlock(data, mimeType, name string, meta map[string]any) Block {
	b := Block{Type: "image", Data: data, MIMEType: mimeType, Name: name}
	if len(meta) > 0 {
		b.Meta = maps.Clone(meta)
	}
	return b
}

// DocumentBlock returns a document block.
func DocumentBlock(data, mimeType, name string, meta map[string]any) Block {
	b := Block{Type: "document", Data: data, MIMEType: mimeType, Name: name}
	if len(meta) > 0 {
		b.Meta = maps.Clone(meta)
	}
	return b
}

// ToolUseBlock returns a tool use block. Input is always a non-nil map to
// match the Python tool_use_block contract (`dict(input or {})`).
func ToolUseBlock(id, name string, input map[string]any, meta map[string]any) Block {
	inputCopy := maps.Clone(input)
	if inputCopy == nil {
		inputCopy = map[string]any{}
	}
	block := Block{Type: "tool_use", ID: id, Name: name, Input: inputCopy}
	if len(meta) > 0 {
		block.Meta = maps.Clone(meta)
	}
	return block
}

// ToolResultBlock returns a tool result block.
func ToolResultBlock(toolUseID, output string, metadata map[string]any, isError bool, content []Block, meta map[string]any) Block {
	block := Block{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Output:    output,
		IsError:   Bool(isError),
	}
	if len(metadata) > 0 {
		block.Metadata = maps.Clone(metadata)
	}
	if len(content) > 0 {
		block.Content = slices.Clone(content)
	}
	if len(meta) > 0 {
		block.Meta = maps.Clone(meta)
	}
	return block
}

// BuildMessage returns a canonical message.
func BuildMessage(role string, blocks []Block, meta map[string]any) Message {
	msg := Message{Role: role}
	if len(blocks) > 0 {
		msg.Content = slices.Clone(blocks)
	}
	if len(meta) > 0 {
		msg.Meta = maps.Clone(meta)
	}
	return msg
}

// UserTextMessage returns a text-only user message.
func UserTextMessage(text string, meta map[string]any) Message {
	return BuildMessage("user", []Block{TextBlock(text, nil)}, meta)
}

// AssistantMessage returns a normalized assistant message.
func AssistantMessage(blocks []Block, provider, model, providerMessageID, stopReason string, usage any, nativeMeta map[string]any) Message {
	meta := map[string]any{}
	if provider != "" {
		meta["provider"] = provider
	}
	if model != "" {
		meta["model"] = model
	}
	if providerMessageID != "" {
		meta["provider_message_id"] = providerMessageID
	}
	if stopReason != "" {
		meta["stop_reason"] = stopReason
	}
	if usage != nil {
		meta["usage"] = usage
	}
	if len(nativeMeta) > 0 {
		meta["native"] = maps.Clone(nativeMeta)
	}
	return BuildMessage("assistant", blocks, meta)
}

// FlattenText returns readable text while skipping attachment snapshots.
func FlattenText(msg Message, includeThinking bool) string {
	parts := make([]string, 0, len(msg.Content))
	for _, block := range msg.Content {
		switch v := block.Meta["attachment"].(type) {
		case bool:
			if v {
				continue
			}
		case string:
			if v == "true" || v == "1" {
				continue
			}
		}
		if block.Type == "text" || (includeThinking && block.Type == "thinking") {
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, " ")
}

// Clone returns a deep-enough copy for replay and persistence.
func Clone(msg Message) Message {
	out := Message{Role: msg.Role}
	if len(msg.Content) > 0 {
		out.Content = CloneBlocks(msg.Content)
	}
	if len(msg.Meta) > 0 {
		out.Meta = maps.Clone(msg.Meta)
	}
	return out
}

// CloneBlock returns a copy of a block.
func CloneBlock(block Block) Block {
	out := block
	if len(block.Input) > 0 {
		out.Input = maps.Clone(block.Input)
	}
	if len(block.Content) > 0 {
		out.Content = CloneBlocks(block.Content)
	}
	if len(block.Metadata) > 0 {
		out.Metadata = maps.Clone(block.Metadata)
	}
	if len(block.Meta) > 0 {
		out.Meta = maps.Clone(block.Meta)
	}
	if block.IsError != nil {
		value := *block.IsError
		out.IsError = &value
	}
	return out
}

// CloneMessages returns a deep copy of a message slice.
func CloneMessages(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	for i, msg := range msgs {
		out[i] = Clone(msg)
	}
	return out
}

// CloneBlocks returns a deep copy of a block slice.
func CloneBlocks(blocks []Block) []Block {
	out := make([]Block, len(blocks))
	for i, block := range blocks {
		out[i] = CloneBlock(block)
	}
	return out
}
