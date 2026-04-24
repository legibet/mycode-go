# Provider Adapters

All adapters live in `mycode-go/internal/provider/`. Provider-specific wire formats stay inside adapters; the agent, session store, server, and web UI only see canonical messages.

## Interface

```go
type Adapter interface {
    Spec() Spec
    StreamTurn(ctx context.Context, req Request) <-chan StreamEvent
}
```

`Request` carries provider, model, session id, canonical messages, system prompt, tool schemas, token limits, API key/base URL, reasoning effort, and image/PDF capability flags.

`StreamTurn` emits normalized events:

- `thinking_delta`
- `text_delta`
- `message_done`
- `provider_error`

`prepareMessages()` in `base.go` handles provider-safe replay:

- skip failed/aborted/cancelled assistant messages
- project tool call ids when a provider restricts ids
- preserve `block.meta.native`
- replace images/PDFs with text notices when the target model cannot accept them
- synthesize interrupted tool results when needed

## Canonical Tool Results

Tool results are stored and replayed as:

```json
{
  "type": "tool_result",
  "tool_use_id": "call_1",
  "output": "Updated file.go",
  "metadata": {"patch": "...", "added_lines": 1, "removed_lines": 1},
  "is_error": false
}
```

`output` is provider-facing text. `metadata` is UI-facing structured data and must not be required for provider replay.

## Built-in Providers

### `anthropic`

- SDK: `github.com/anthropics/anthropic-sdk-go`
- Base URL: `https://api.anthropic.com`
- Env: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`
- Default models: `claude-sonnet-4-6`, `claude-opus-4-7`
- Reasoning effort: supported
- Adaptive thinking for `claude-sonnet-4-6`, `claude-opus-4-6`, and `claude-opus-4-7`
- `claude-opus-4-7` uses summarized adaptive thinking display and forwards `output_config.effort`
- `xhigh` maps to `high` for Sonnet 4.6 and `max` for Opus 4.6
- Adds ephemeral cache control to system and last user content block
- Projects tool call ids to ASCII-safe ids with SHA1 collision suffix
- Images and PDFs are serialized as Anthropic content blocks

### `moonshotai`

- SDK: Anthropic Go SDK against `https://api.moonshot.ai/anthropic`
- Env: `MOONSHOT_API_KEY`
- Default model: `kimi-k2.6`
- Reasoning effort maps to manual Anthropic-style `budget_tokens`
- Prior reasoning blocks are replayed on later tool-loop turns

### `minimax`

- SDK: Anthropic Go SDK against `https://api.minimax.io/anthropic`
- Env: `MINIMAX_API_KEY`
- Default models: `MiniMax-M2.7`, `MiniMax-M2.7-highspeed`
- Reasoning effort maps to manual Anthropic-style `budget_tokens`
- Provider-native thinking signatures are preserved in `block.meta.native`

### `google`

- SDK: `google.golang.org/genai`
- Base URL: `https://generativelanguage.googleapis.com`
- Env: `GEMINI_API_KEY`, `GOOGLE_API_KEY`
- Default models: `gemini-3.1-pro-preview`, `gemini-3-flash-preview`
- Reasoning effort: supported for Gemini 3
- `none`/`low` map to `LOW` for `gemini-3.1-pro*`, `MINIMAL` for other Gemini 3 models
- `medium` maps to `MEDIUM`; `high`/`xhigh` map to `HIGH`
- Replays native `Part` metadata from `block.meta.native.part`
- Adds the documented dummy thought signature for cross-provider replay when needed
- Uses a Go context timeout rather than GenAI HTTPOptions timeout to avoid premature stream cancellation

### `openai`

- SDK: `github.com/openai/openai-go/v3`
- API: Responses
- Env: `OPENAI_API_KEY`
- Default models: `gpt-5.4`, `gpt-5.4-mini`
- Reasoning effort: supported
- Runs stateless with `store=false`
- Includes encrypted reasoning content
- Persists completed Responses output items under `assistant.meta.native.output_items` for direct replay
- Tool results replay as `function_call_output.output`
- Uses `prompt_cache_key` from the session id

### `openai_chat`

- SDK: `github.com/openai/openai-go/v3`
- API: Chat Completions
- Env: `OPENAI_API_KEY`
- Default models: `gpt-5.4`, `gpt-5.4-mini`
- Auto-discovery: disabled
- Intended for OpenAI-compatible gateways that do not support Responses
- Preserves reasoning extensions exposed by compatible providers
- Sends `stream_options.include_usage=true`

### `deepseek`

- Chat Completions adapter
- Base URL: `https://api.deepseek.com`
- Env: `DEEPSEEK_API_KEY`
- Default models: `deepseek-v4-pro`, `deepseek-v4-flash`
- Reasoning effort: `none` sends `thinking: {type: "disabled"}`, `low`/`medium`/`high` map to `reasoning_effort=high`, and `xhigh` maps to `reasoning_effort=max`

### `zai`

- Chat Completions adapter
- Base URL: `https://api.z.ai/api/paas/v4`
- Env: `ZAI_API_KEY`
- Default models: `glm-5.1`, `glm-5-turbo`
- Reasoning effort: not exposed
- Sends provider-specific thinking config with `clear_thinking=false`

### `openrouter`

- Chat Completions adapter
- Base URL: `https://openrouter.ai/api/v1`
- Env: `OPENROUTER_API_KEY`
- Default model: `openrouter/auto`
- Reasoning effort is forwarded through OpenRouter's `reasoning.effort`

## Reasoning Effort Mapping

| effort   | anthropic / moonshotai / minimax     | google (Gemini 3)  | openai / openrouter | deepseek |
| -------- | ------------------------------------ | ------------------ | ------------------- | -------- |
| `none`   | disabled                             | `LOW` or `MINIMAL` | `none`              | disabled |
| `low`    | low budget                           | `LOW` or `MINIMAL` | `low`               | `high`   |
| `medium` | medium budget                        | `MEDIUM`           | `medium`            | `high`   |
| `high`   | high budget                          | `HIGH`             | `high`              | `high`   |
| `xhigh`  | provider-specific high/max behaviour | `HIGH`             | `xhigh`             | `max`    |

Effort is only applied when both `Spec.SupportsReasoningEffort` and catalog `supports_reasoning` are true.

## Sync Rules

When Python main changes provider behavior:

- Keep canonical message/session formats identical.
- Implement the external behavior in idiomatic Go.
- Do not copy Python architecture into Go when a simpler Go implementation is sufficient.
- Keep provider quirks inside adapter files.
