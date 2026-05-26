# Provider Adapters

All adapters live in `internal/provider/`. Provider-specific wire formats stay inside adapters; the agent, session store, server, and web UI only see canonical messages.

## Interface

```go
type Adapter interface {
    Spec() Spec
    StreamTurn(ctx context.Context, req Request) <-chan StreamEvent
}
```

`StreamTurn` emits normalized events:

- `thinking_delta` — reasoning text
- `text_delta` — response text
- `message_done` — final canonical assistant message with all blocks and metadata
- `provider_error` — provider/runtime failure

`Request` carries provider, model, session id, messages, system prompt, tools, token limits, api key, api base, reasoning effort, image support, and PDF support.

`prepareMessages()` in `base.go` converts canonical session history to provider-safe replay:

- skip assistant messages with `stop_reason` in `{error, aborted, cancelled}`
- project tool call IDs when a provider restricts ids
- preserve `block.meta.native` for provider-specific replay data; local metadata such as `duration_ms` is not sent upstream
- replace replay images with a short text notice when the target model cannot accept them
- replace replay PDFs with a short text notice when the target model cannot accept them
- insert synthetic error tool results when pending tool calls would otherwise make replay invalid

## Adapters

### `anthropic` — `anthropic.go`

- SDK: `github.com/anthropics/anthropic-sdk-go`
- API: Anthropic Messages API
- Base URL: `https://api.anthropic.com`
- API key env: `ANTHROPIC_API_KEY`
- Default models: `claude-sonnet-4-6`, `claude-opus-4-7`
- `SupportsReasoningEffort`: true
- Adaptive thinking for `claude-sonnet-4-6`, `claude-opus-4-6`, and `claude-opus-4-7`; manual `budget_tokens` for older reasoning models
- `claude-opus-4-7` uses adaptive thinking and forwards `output_config.effort`
- `reasoning_effort=xhigh` maps to `high` for Sonnet 4.6 and `max` for Opus 4.6
- Adds ephemeral `cache_control` to the system prompt block and latest user content block
- Tool call IDs are projected to ASCII-safe format with a SHA1 collision suffix
- Images serialize as Anthropic `image` blocks
- PDFs serialize as Anthropic `document` blocks

### `moonshotai` — `anthropic.go`

- SDK: `github.com/anthropics/anthropic-sdk-go` against Moonshot's Anthropic-compatible endpoint
- Base URL: `https://api.moonshot.ai/anthropic`
- API key env: `MOONSHOT_API_KEY`
- Default model: `kimi-k2.6`
- `SupportsReasoningEffort`: true; maps to manual `budget_tokens`
- Prior reasoning blocks are replayed on later tool-loop turns when thinking is enabled
- Shares Anthropic-like ephemeral cache markers, tool call ID projection, image format, and PDF format

### `minimax` — `anthropic.go`

- SDK: `github.com/anthropics/anthropic-sdk-go` against MiniMax's Anthropic-compatible endpoint
- Base URL: `https://api.minimax.io/anthropic`
- API key env: `MINIMAX_API_KEY`
- Default models: `MiniMax-M2.7`, `MiniMax-M2.7-highspeed`
- `SupportsReasoningEffort`: true; maps to manual `budget_tokens`
- Preserves provider-native thinking signatures in `block.meta.native`
- Shares Anthropic-like ephemeral cache markers, tool call ID projection, image format, and PDF format

### `google` — `google.go`

- SDK: `google.golang.org/genai`
- API: Gemini Developer API
- Base URL: `https://generativelanguage.googleapis.com`
- API key env: `GEMINI_API_KEY`, `GOOGLE_API_KEY`
- Default models: `gemini-3.5-flash`, `gemini-3.1-pro-preview`
- `SupportsReasoningEffort`: true for Gemini 3 models through `thinking_level`
- Reasoning effort mapping for Gemini 3:
  - `none` -> `LOW` for `gemini-3.1-pro*`, `MINIMAL` for other `gemini-3*` models
  - `low` -> `LOW`
  - `medium` -> `MEDIUM`
  - `high`/`xhigh` -> `HIGH`
- Replays native `Part` metadata through `block.meta.native.part`, preserving function-call ids and thought signatures
- Cross-provider tool-loop fallback adds the documented dummy thought signature when needed
- Empty-text streaming parts that carry thought signatures are persisted
- Gemini validates function_call id/name match between function_call and function_response pairs
- `thinking_config.include_thoughts` is true; effort level controls `thinking_level`
- Images and PDFs serialize as `inline_data`

### `openai` — `openai_responses.go`

- SDK: `github.com/openai/openai-go/v3`
- API: OpenAI Responses API
- Base URL: `https://api.openai.com/v1`
- API key env: `OPENAI_API_KEY`
- Default models: `gpt-5.5`, `gpt-5.4-mini`
- `SupportsReasoningEffort`: true; values are `none`, `low`, `medium`, `high`, `xhigh`
- Runs stateless with `store=false`
- Includes encrypted reasoning content
- Persists completed Responses output items under `assistant.meta.native.output_items` for direct replay
- Tool results replay as `function_call_output.output`
- Passes `prompt_cache_key` using the current session id
- Tool schemas use `strict: true` with nullable optional parameters
- Images serialize as `input_image`
- PDFs serialize as `input_file`

### `openai_chat` — `openai_chat.go`

- SDK: `github.com/openai/openai-go/v3`
- API: OpenAI Chat Completions
- `SupportsReasoningEffort`: false
- `AutoDiscoverable`: false
- Intended for third-party OpenAI-compatible providers when Responses API is unavailable
- Preserves third-party reasoning extensions from SDK extras:
  - `reasoning` replays as `reasoning`
  - `reasoning_content` replays as `reasoning_content`, including empty field markers
  - `reasoning_details` replays as `reasoning_details`
- Empty provider-native reasoning blocks are retained for replay even when no reasoning text was shown to the user
- Sends `stream_options.include_usage=true`
- Images serialize as `image_url` parts with data URLs
- PDFs serialize as `file` parts with base64 data URLs

### `deepseek` — `openai_chat.go`

- SDK: `github.com/openai/openai-go/v3` against DeepSeek's OpenAI-compatible endpoint
- Base URL: `https://api.deepseek.com`
- API key env: `DEEPSEEK_API_KEY`
- Default models: `deepseek-v4-pro`, `deepseek-v4-flash`
- `SupportsReasoningEffort`: true; `none` sends `thinking: {type: "disabled"}`, `low`/`medium`/`high` map to `reasoning_effort=high`, and `xhigh` maps to `reasoning_effort=max`
- `AutoDiscoverable`: true
- Stored `reasoning_content` is replayed on later requests, including empty markers after tool turns

### `zai` — `openai_chat.go`

- SDK: `github.com/openai/openai-go/v3` against Z.AI's OpenAI-compatible endpoint
- Base URL: `https://api.z.ai/api/paas/v4`
- API key env: `ZAI_API_KEY`
- Default models: `glm-5.1`, `glm-5-turbo`
- `SupportsReasoningEffort`: false; thinking is enabled by provider-specific config with `clear_thinking=false`
- `AutoDiscoverable`: true
- `clear_thinking=false` preserves reasoning across multi-turn tool loops; historical `reasoning_content` must be replayed unmodified

### `openrouter` — `openai_chat.go`

- SDK: `github.com/openai/openai-go/v3` against OpenRouter's OpenAI-compatible endpoint
- Base URL: `https://openrouter.ai/api/v1`
- API key env: `OPENROUTER_API_KEY`
- Default model: `openrouter/auto`
- `SupportsReasoningEffort`: true; forwarded through OpenRouter's `reasoning.effort`
- `AutoDiscoverable`: true
- Supports OpenRouter reasoning replay shapes: `reasoning`, `reasoning_content`, and `reasoning_details`
- Same image format as `openai_chat`
- Same PDF format as `openai_chat`

## Reasoning Effort Mapping

| effort   | anthropic / moonshotai / minimax       | google (Gemini 3)       | openai / openrouter | deepseek |
| -------- | -------------------------------------- | ----------------------- | ------------------- | -------- |
| `none`   | thinking disabled                      | `LOW`/`MINIMAL` level   | `none`              | disabled |
| `low`    | low `budget_tokens`                    | `LOW`/`MINIMAL` level   | `low`               | `high`   |
| `medium` | medium `budget_tokens`                 | `MEDIUM` level          | `medium`            | `high`   |
| `high`   | high `budget_tokens`                   | `HIGH` level            | `high`              | `high`   |
| `xhigh`  | `high` (sonnet) / `max` (opus) effort  | `HIGH` level            | `xhigh`             | `max`    |

Config-resolved `reasoning_effort` is only applied when both `Spec.SupportsReasoningEffort` and catalog `supports_reasoning` are true.

## Message Replay

`prepareMessages()` in `base.go` handles the canonical -> provider wire format projection:

1. Skip assistant messages with `stop_reason` in `{error, aborted, cancelled}`.
2. Project tool call IDs to provider-safe format. Only Anthropic-like adapters override this.
3. Preserve `block.meta.native` for provider-specific replay data such as signatures, output items, and part metadata. Local metadata such as `duration_ms` is not sent upstream.
4. Replace replay images with a short text notice when `request.SupportsImageInput` is false.
5. Replace replay PDFs with a short text notice when `request.SupportsPDFInput` is false.
6. Insert synthetic error tool results when pending tool calls would otherwise make replay invalid.

Provider-specific replay logic lives inside each adapter's serialization functions. These run after `prepareMessages()` produces the canonical replay transcript.

For OpenAI-compatible chat providers, empty `thinking` blocks with `block.meta.native` are intentionally preserved. Some providers require a reasoning field to be returned even when its value is empty or null.
