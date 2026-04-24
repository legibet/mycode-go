# Sessions

Source: `mycode-go/internal/session/store.go`

The on-disk format matches Python `mycode-sdk` / `mycode-cli` message format version 6.

## Storage Layout

```text
$MYCODE_HOME/sessions/<session_id>/
  meta.json        # session metadata
  messages.jsonl   # one JSON record per line, append-only
  tool-output/     # bash spill files, created lazily
```

`$MYCODE_HOME` defaults to `~/.mycode`. `tool-output/` is created only when bash output is actually spilled.

## meta.json

```json
{
  "cwd": "/path/to/workspace",
  "title": "New chat",
  "created_at": "...",
  "updated_at": "...",
  "message_format_version": 6
}
```

- `id` is not persisted. API responses add it from the directory name.
- `provider`, `model`, and `api_base` are not persisted in meta. Per-turn provider state lives on assistant message `meta`.
- `title` starts as `"New chat"` and is promoted to the first readable user message, truncated to 48 chars.
- `updated_at` is bumped on every appended message.
- `message_format_version` is written as `6`.

## Message Blocks

Canonical messages are block-based:

```json
{"role": "user", "content": [{"type": "text", "text": "..."}]}
{"role": "assistant", "content": [{"type": "thinking", "text": "..."}, {"type": "text", "text": "..."}, {"type": "tool_use", "id": "call_1", "name": "read", "input": {"path": "x.go"}}], "meta": {"provider": "...", "model": "...", "usage": {...}}}
{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1", "output": "...", "metadata": {...}, "is_error": false}]}
```

`tool_result.output` is replayed to providers. `tool_result.metadata` is structured UI data. The `edit` tool stores a unified `patch` plus `added_lines` / `removed_lines`.

## Record Types

### Regular Message

`role` is `user` or `assistant`. User messages may contain `text`, `image`, `document`, and `tool_result`. Assistant messages may contain `thinking`, `text`, and `tool_use`.

### Compact Event

```json
{"role": "compact", "content": [{"type": "text", "text": "<summary>"}], "meta": {"provider": "...", "model": "...", "compacted_count": 12}}
```

### Rewind Event

```json
{"role": "rewind", "meta": {"rewind_to": 5, "created_at": "..."}}
```

## Load Order

`Store.LoadSession(sessionID)` applies the same replay order as Python:

1. Read raw JSONL records.
2. Apply latest compact summary.
3. Apply rewind markers sequentially.
4. Repair an interrupted final tool loop by appending synthetic error `tool_result` blocks.

## Context Compaction

After a successful turn, the agent checks the last assistant usage. Compaction triggers when:

```text
usage.input_tokens >= context_window * compact_threshold
```

Fallback usage fields are `prompt_tokens` and `prompt_token_count`.

The compact request uses the same provider/model, no tools, and no explicit reasoning effort. The compact event is appended; older JSONL records are never rewritten.

## Rewind

`POST /api/chat` with `rewind_to` validates that the target visible message is a real user prompt. The server appends a rewind marker before starting the replacement run. Loading the session later produces the same visible history in Python and Go.

## Bash Output Spill

Bash keeps up to 5 MB in memory. Larger output spills to:

```text
<session_dir>/tool-output/bash-<tool_call_id>.log
```

The returned `output` contains the visible tail and a "Full output" path.

## Store API

Important methods:

- `CreateSession(sessionID, cwd)` creates `meta.json` and `messages.jsonl`.
- `DraftSession(cwd)` creates an in-memory summary with a fresh id.
- `ListSessions(cwd)` returns summaries sorted by `updated_at` descending.
- `LoadSession(sessionID)` returns visible messages after replay.
- `AppendMessage(sessionID, msg, cwd)` appends one message and refreshes meta.
- `AppendRewind(sessionID, rewindTo)` appends a rewind marker if the session exists.
- `ClearSession(sessionID)` truncates messages and resets title.
- `DeleteSession(sessionID)` removes the session directory.

The store remains append-only for conversation history. Only `meta.json` and explicit clear/delete operations mutate existing files.
