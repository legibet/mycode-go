# Sessions

Source: `session/store.go`

## Storage Layout

```text
$MYCODE_HOME/sessions/
  index.json       # session list cache
  <session_id>/
    meta.json      # session metadata
    messages.jsonl # one JSON record per line, append-only
    tool-output/   # bash spill files, created lazily
```

`$MYCODE_HOME` defaults to `~/.mycode`. `tool-output/` is the per-session directory passed into tool execution and is created only when bash output spills.

## meta.json

```json
{
  "cwd": "/path/to/workspace",
  "title": "...",
  "created_at": "...",
  "updated_at": "..."
}
```

- `cwd` — workspace path recorded at session creation; used by `ListSessions(cwd)` for filtering
- `title` — defaults to `"New chat"`; promoted to the first user message text, truncated to 48 chars
- `updated_at` — bumped on every appended message

Per-turn state (`provider` / `model` / `api_base`) lives only on each assistant message `meta`; caching a current value at the session level would drift after model switches.

## index.json

```json
{
  "session-id": {
    "cwd": "/path/to/workspace",
    "title": "...",
    "created_at": "...",
    "updated_at": "..."
  }
}
```

`index.json` maps session ids to session metadata. `ListSessions(cwd)` reads it directly; missing or invalid index data is rebuilt from existing `meta.json` files.

## messages.jsonl Record Types

Each line is a JSON object. The `role` field acts as a discriminator.

### Regular message

Standard `user` or `assistant` message in the internal block format.

```json
{"role": "user", "content": [{"type": "text", "text": "..."}], "meta": {...}}
{"role": "assistant", "content": [{"type": "thinking", "text": "...", "meta": {"duration_ms": 1200}}, {"type": "text", "text": "..."}, {"type": "tool_use", "id": "...", "name": "...", "input": {...}}], "meta": {"provider": "...", "model": "...", "stop_reason": "...", "total_tokens": 1456, "context_window": 200000}}
```

User messages may contain `text`, `image`, `document`, and `tool_result` blocks. Assistant messages may contain `thinking`, `text`, and `tool_use` blocks.

`tool_result.output` is replayed to providers. `tool_result.metadata` is structured UI data. The `edit` tool stores a unified `patch` plus `added_lines` / `removed_lines`.

`assistant.meta.total_tokens` is the canonical token count for the provider call: prompt plus generated text, tool calls, and reasoning. `assistant.meta.context_window` is stamped from the resolved model metadata so clients can render consumed-context percentage without resolving the model again.

Cancelled provider streams can append a partial assistant message before the final cancelled error. It contains streamed `thinking`/`text` blocks plus provider/model/context metadata.

### Compact event

```json
{"role": "compact", "content": [{"type": "text", "text": "<summary>"}], "meta": {"provider": "...", "model": "...", "total_tokens": 48000}}
```

Inline marker written when token usage >= `compact_threshold * context_window`. The summary text is persisted in the marker; UIs render it as a divider in the visible history.

### Rewind event

```json
{"role": "rewind", "meta": {"rewind_to": 5, "created_at": "..."}}
```

Marks an undo point. See "Rewind" below.

## Load Order

When `Store.LoadSession(sessionID)` runs:

1. Read all JSONL lines into a raw list.
2. Apply rewind markers sequentially.

`LoadSession` returns the raw timeline minus rewound tails as the visible history. `compact` records stay in place as inline markers. The agent substitutes the latest compact marker into provider context lazily on each request via `ApplyCompactReplay`. Orphan `tool_use` blocks left by an interrupted run are closed by the provider adapter when messages are replayed, not by the loader.

## Context Compaction

Checked after every completed assistant turn, at a full assistant/tool-result boundary.

1. `shouldCompact()` is true when the latest assistant message's `total_tokens` >= `context_window * compact_threshold`.
2. Ask the same provider/model for a summary with the normal system prompt, the provider-projected messages, no tools, and no explicit reasoning effort.
3. Build a compact event with the summary text and the summary call's `total_tokens` when available.
4. Persist the compact event and append it to in-memory messages. Original messages stay in JSONL and in the visible list.
5. Emit the `compact` stream event to the caller.

If the summary request fails or returns no text, no `compact` record is persisted and the next threshold check can try again. A user cancel during compaction ends the turn with `error: cancelled`.

### Provider projection

Visible state preserves pre-compact history and `compact` markers. Before each provider request, `ApplyCompactReplay` rebuilds a provider-facing view:

- finds the last `compact` marker
- replaces everything up to and including that marker with one synthetic `user` message that frames the continuation, embeds the summary text, and points at the original JSONL transcript when available
- drops any earlier `compact` markers from the tail
- inserts a short synthetic `assistant` ack only when the tail starts with a real user message and role alternation requires it

The visible list seen by UIs and used by rewind never contains these synthetic substitutes.

## Rewind

Triggered by `POST /api/chat` with `rewind_to`:

1. Server validates the target is a real user message.
2. Server calls `AppendRewind(sessionID, rewindTo)` and appends a rewind marker to JSONL.
3. The agent resumes from `ApplyRewind` visible history.

Rewind indices refer to the visible history, which includes pre-compact turns and inline `compact` markers. Rewinding to a real user message before a `compact` marker slices the marker away with the rest of the tail. A compact marker itself is never a valid rewind target.

## tool-output/ Spill

Bash output exceeding 5MB in memory is written to:

```text
<session_dir>/tool-output/bash-<tool_call_id>.log
```

The tool result keeps the visible tail in memory and cites the saved log path.

For memory-only SDK agents with no `SessionDir`, bash spill files are written under the system temp directory:

```text
<tmp>/mycode/<session_id>/tool-output/bash-<tool_call_id>.log
```

Cancelled streaming tools persist emitted output plus `error: cancelled`.

## Session Store API

`Store` in `session/store.go`:

- `NewStore(dataDir)` — `dataDir` required; creates the directory and returns an error on failure
- `SessionExists(sessionID)` — check by `meta.json` presence
- `CreateSession(sessionID, cwd)` — write `meta.json` and touch `messages.jsonl`
- `DraftSession(cwd)` — create an in-memory session summary with a fresh id
- `ListSessions(cwd)` — filter by workspace, sorted by `updated_at` descending
- `LoadSession(sessionID)` — load with rewind replay; returns `nil` when absent
- `DeleteSession(sessionID)` — recursive directory delete
- `ClearSession(sessionID)` — truncate `messages.jsonl`, reset `title`, and bump `updated_at`
- `AppendMessage(sessionID, msg, cwd)` — append one line, refresh meta, and promote title on the first readable user text
- `AppendRewind(sessionID, rewindTo)` — append a rewind marker

The store remains append-only for conversation history. Only `meta.json` and explicit clear/delete operations mutate existing files.
