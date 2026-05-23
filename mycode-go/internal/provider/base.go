package provider

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/legibet/mycode-go/internal/message"
)

const defaultRequestTimeout = 300 * time.Second

// Request is one provider turn request.
type Request struct {
	Provider           string
	Model              string
	SessionID          string
	Messages           []message.Message
	System             string
	Tools              []map[string]any
	MaxTokens          int
	APIKey             string
	APIBase            string
	ReasoningEffort    string
	SupportsImageInput bool
	SupportsPDFInput   bool
}

// StreamEvent is the normalized provider stream event.
type StreamEvent struct {
	Type string
	Text string
	Msg  *message.Message
	Err  error
}

// Adapter streams one provider turn.
type Adapter interface {
	Spec() Spec
	StreamTurn(ctx context.Context, req Request) <-chan StreamEvent
}

type baseAdapter struct {
	spec Spec
}

func (b baseAdapter) Spec() Spec {
	return b.spec
}

func prepareMessages(req Request, projectToolCallID func(string, map[string]struct{}) string) []message.Message {
	repaired := RepairMessagesForReplay(req.Messages, req.SupportsImageInput, req.SupportsPDFInput)
	prepared := make([]message.Message, 0, len(repaired))
	toolIDs := map[string]string{}
	usedProjected := map[string]struct{}{}
	for _, msg := range repaired {
		next := message.Clone(msg)
		for i := range next.Content {
			block := next.Content[i]
			switch block.Type {
			case "tool_use":
				if block.ID != "" {
					if _, ok := toolIDs[block.ID]; !ok {
						toolIDs[block.ID] = projectToolCallID(block.ID, usedProjected)
						usedProjected[toolIDs[block.ID]] = struct{}{}
					}
					next.Content[i].ID = toolIDs[block.ID]
				}
			case "tool_result":
				if projected, ok := toolIDs[block.ToolUseID]; ok {
					next.Content[i].ToolUseID = projected
				}
			}
		}
		prepared = append(prepared, next)
	}
	return prepared
}

func defaultProjectToolCallID(toolCallID string, _ map[string]struct{}) string {
	return toolCallID
}

// RepairMessagesForReplay converts canonical history into a replay-safe transcript.
func RepairMessagesForReplay(source []message.Message, supportsImageInput, supportsPDFInput bool) []message.Message {
	replay := make([]message.Message, 0, len(source))
	seenToolUseIDs := map[string]struct{}{}
	seenToolResultIDs := map[string]struct{}{}
	pendingToolUseIDs := []string{}

	for _, msg := range source {
		switch msg.Role {
		case "assistant":
			if len(pendingToolUseIDs) > 0 {
				replay = append(replay, interruptedToolResultMessage(pendingToolUseIDs))
				for _, id := range pendingToolUseIDs {
					seenToolResultIDs[id] = struct{}{}
				}
				pendingToolUseIDs = nil
			}

			stopReason, _ := msg.Meta["stop_reason"].(string)
			if stopReason == "error" || stopReason == "aborted" || stopReason == "cancelled" {
				continue
			}

			nextContent := make([]message.Block, 0, len(msg.Content))
			nextPendingToolUseIDs := make([]string, 0)
			for _, block := range msg.Content {
				switch block.Type {
				case "text", "thinking":
					if strings.TrimSpace(block.Text) == "" && len(blockNativeMeta(block)) == 0 {
						continue
					}
					nextContent = append(nextContent, message.CloneBlock(block))
				case "tool_use":
					if block.ID == "" {
						continue
					}
					if _, seen := seenToolUseIDs[block.ID]; seen {
						continue
					}
					seenToolUseIDs[block.ID] = struct{}{}
					nextPendingToolUseIDs = append(nextPendingToolUseIDs, block.ID)
					nextContent = append(nextContent, message.CloneBlock(block))
				}
			}
			if len(nextContent) == 0 {
				continue
			}

			next := message.Clone(msg)
			next.Content = nextContent
			replay = append(replay, next)
			pendingToolUseIDs = nextPendingToolUseIDs
		case "user":
			nextContent := make([]message.Block, 0, len(msg.Content))
			resolvedToolUseIDs := map[string]struct{}{}
			hasVisibleUserInput := false
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) == "" {
						continue
					}
					hasVisibleUserInput = true
					nextContent = append(nextContent, message.CloneBlock(block))
				case "image", "document":
					hasVisibleUserInput = true
					supported := supportsImageInput
					label := "image input"
					defaultMIME := "image"
					if block.Type == "document" {
						supported = supportsPDFInput
						label = "PDF input"
						defaultMIME = "application/pdf"
					}
					if supported {
						nextContent = append(nextContent, message.CloneBlock(block))
						continue
					}
					attachment := fmt.Sprintf(
						"<file name=\"%s\" media_type=\"%s\" kind=\"%s\">Current model does not support %s.</file>",
						message.EscapeXMLAttr(cmp.Or(block.Name, "attached-"+block.Type)),
						message.EscapeXMLAttr(cmp.Or(block.MIMEType, defaultMIME)),
						block.Type,
						label,
					)
					nextContent = append(nextContent, message.TextBlock(attachment, map[string]any{"attachment": true}))
				case "tool_result":
					if block.ToolUseID == "" {
						continue
					}
					if _, seen := seenToolUseIDs[block.ToolUseID]; !seen {
						continue
					}
					if _, seen := seenToolResultIDs[block.ToolUseID]; seen {
						continue
					}
					next := message.CloneBlock(block)
					if !supportsImageInput && len(next.Content) > 0 {
						next.Content = slices.DeleteFunc(next.Content, func(child message.Block) bool {
							return child.Type == "image"
						})
					}
					nextContent = append(nextContent, next)
					resolvedToolUseIDs[block.ToolUseID] = struct{}{}
					seenToolResultIDs[block.ToolUseID] = struct{}{}
				}
			}

			if len(pendingToolUseIDs) > 0 {
				unresolvedToolUseIDs := make([]string, 0, len(pendingToolUseIDs))
				for _, id := range pendingToolUseIDs {
					if _, ok := resolvedToolUseIDs[id]; !ok {
						unresolvedToolUseIDs = append(unresolvedToolUseIDs, id)
					}
				}
				if hasVisibleUserInput {
					if len(unresolvedToolUseIDs) > 0 {
						replay = append(replay, interruptedToolResultMessage(unresolvedToolUseIDs))
						for _, id := range unresolvedToolUseIDs {
							seenToolResultIDs[id] = struct{}{}
						}
					}
					pendingToolUseIDs = nil
				} else {
					pendingToolUseIDs = unresolvedToolUseIDs
				}
			}

			if len(nextContent) == 0 {
				if len(replay) > 0 && replay[len(replay)-1].Role == "assistant" {
					replay = append(replay, message.BuildMessage("user", []message.Block{
						message.TextBlock("[User turn omitted during replay]", nil),
					}, map[string]any{"synthetic": true}))
				}
				continue
			}

			next := message.Clone(msg)
			next.Content = nextContent
			replay = append(replay, next)
		}
	}

	if len(pendingToolUseIDs) > 0 {
		replay = append(replay, interruptedToolResultMessage(pendingToolUseIDs))
	}

	return replay
}

// ToolResultContentBlocks returns structured tool result content or falls back to one text block.
func ToolResultContentBlocks(block message.Block) []message.Block {
	if len(block.Content) > 0 {
		return message.CloneBlocks(block.Content)
	}
	return []message.Block{message.TextBlock(block.Output, nil)}
}

func interruptedToolResultMessage(toolUseIDs []string) message.Message {
	blocks := make([]message.Block, 0, len(toolUseIDs))
	for _, toolUseID := range toolUseIDs {
		blocks = append(blocks, message.ToolResultBlock(
			toolUseID,
			"error: tool call was interrupted",
			nil,
			true,
			nil,
			nil,
		))
	}
	return message.BuildMessage("user", blocks, nil)
}

func blockNativeMeta(block message.Block) map[string]any {
	raw, _ := block.Meta["native"].(map[string]any)
	if raw == nil {
		return map[string]any{}
	}
	return raw
}

func messageNativeMeta(msg message.Message) map[string]any {
	raw, _ := msg.Meta["native"].(map[string]any)
	if raw == nil {
		return map[string]any{}
	}
	return raw
}

func loadImageBlockPayload(block message.Block) (mimeType, data string, err error) {
	if block.MIMEType == "" {
		return "", "", fmt.Errorf("image block is missing mime_type")
	}
	if block.Data == "" {
		return "", "", fmt.Errorf("image block is missing data")
	}
	return block.MIMEType, block.Data, nil
}

func loadDocumentBlockPayload(block message.Block) (mimeType, data, name string, err error) {
	if block.MIMEType == "" {
		return "", "", "", fmt.Errorf("document block is missing mime_type")
	}
	if block.Data == "" {
		return "", "", "", fmt.Errorf("document block is missing data")
	}
	return block.MIMEType, block.Data, block.Name, nil
}

func decodeBase64(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	return decoded
}

func dumpJSON(value any) any {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func dumpJSONMap(value any) map[string]any {
	out, _ := dumpJSON(value).(map[string]any)
	return out
}

func tokenCount(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case int32:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		parsed, _ := v.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func mapTokenCount(value any, keys ...string) int {
	raw, _ := value.(map[string]any)
	if raw == nil {
		return 0
	}
	for _, key := range keys {
		if tokens := tokenCount(raw[key]); tokens > 0 {
			return tokens
		}
	}
	return 0
}

func unmarshalJSONMap(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
