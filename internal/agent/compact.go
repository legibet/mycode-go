// Context compaction: summarize past history when nearing the context window.
// Compact records persist inline as `role: "compact"` markers; ApplyCompactReplay
// substitutes them with a summary continuation only when projecting messages
// for the provider.

package agent

import (
	"fmt"
	"strings"

	"github.com/legibet/mycode-go/internal/message"
)

const CompactSummaryPrompt = `Summarize this conversation to create a continuation document. This summary will replace the full conversation history, so it must capture everything needed to continue the work seamlessly.

Include:

1. **Task and Intent**: Describe the user's overall goal — what is being built, fixed, or investigated, and why.
2. **Decisions and Constraints**: List the decisions made, constraints discovered, and approaches chosen or rejected, with the reasoning behind each.
3. **User Requests**: Every distinct request or instruction the user gave, in chronological order. Preserve the user's original wording for ambiguous or nuanced requests.
4. **Files and Changes**: Enumerate every file read, modified, or created — paths, what changed, and any code snippets the next turn will need to reason about, quoted verbatim.
5. **Errors and Fixes**: List errors encountered with the original message verbatim, the cause if known, and the resolution — or that it remains open.
6. **Current State**: What is verified working, what is known broken, what is in progress.
7. **Next Step**: The next step to take, with a direct quote from the most recent conversation showing where the work left off.

Rules:
- Be specific: reproduce file paths, function names, error messages, and other identifiers verbatim — never paraphrase them.
- Do not add suggestions or opinions — only summarize what happened.
- Keep it concise but complete.`

const continuationHeader = "This session is being continued from a previous conversation that was compacted to fit the context window. The summary below covers the earlier portion of the conversation."

const transcriptHintTemplate = "For verbatim details not captured in this summary (exact code snippets, error messages, or earlier output), read the original conversation log at: %s"

const continuationFooter = `Resume directly from where the work left off. Do not acknowledge this summary, do not recap, and do not preface with "I'll continue" or similar.`

const compactAck = "Acknowledged."

func ShouldCompact(totalTokens, contextWindow int, threshold float64) bool {
	if totalTokens <= 0 || contextWindow <= 0 || threshold <= 0 {
		return false
	}
	return float64(totalTokens) >= float64(contextWindow)*threshold
}

func BuildCompactEvent(summary, provider, model string, totalTokens int) message.Message {
	meta := map[string]any{
		"provider": provider,
		"model":    model,
	}
	if totalTokens > 0 {
		meta["total_tokens"] = totalTokens
	}
	return message.BuildMessage("compact", []message.Block{message.TextBlock(summary, nil)}, meta)
}

// ApplyCompactReplay rewrites messages so the latest `compact` marker becomes
// a summary continuation; the input slice is not mutated.
//
// The tail (messages after the latest compact) drives whether we follow the
// summary with an "Acknowledged." assistant turn: an assistant-led tail (or
// none) means we resume directly; a user-led tail needs the ack so role
// alternation stays valid for the provider.
func ApplyCompactReplay(messages []message.Message, transcriptPath string) []message.Message {
	lastCompact := -1
	for i, msg := range messages {
		if msg.Role == "compact" {
			lastCompact = i
		}
	}
	if lastCompact < 0 {
		return messages
	}

	summary := ""
	for _, block := range messages[lastCompact].Content {
		if block.Type == "text" {
			summary = block.Text
			break
		}
	}

	tail := make([]message.Message, 0, len(messages)-lastCompact-1)
	for _, msg := range messages[lastCompact+1:] {
		if msg.Role == "compact" {
			continue
		}
		tail = append(tail, msg)
	}
	continueNow := len(tail) == 0 || tail[0].Role == "assistant"

	parts := []string{continuationHeader, summary}
	if transcriptPath != "" {
		parts = append(parts, fmt.Sprintf(transcriptHintTemplate, transcriptPath))
	}
	if continueNow {
		parts = append(parts, continuationFooter)
	}

	projected := make([]message.Message, 0, len(tail)+2)
	projected = append(projected, message.UserTextMessage(strings.Join(parts, "\n\n"), nil))
	if !continueNow {
		projected = append(projected, message.BuildMessage("assistant", []message.Block{message.TextBlock(compactAck, nil)}, nil))
	}
	projected = append(projected, tail...)
	return projected
}
