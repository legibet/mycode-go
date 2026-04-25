# Server API

Base prefix: `/api`. Routes are wired in `mycode-go/internal/server/app.go`; request parsing and handlers live in `mycode-go/internal/server/*.go`.

The API matches the Python web contract from `main` so the same browser session can switch between Python and Go backends.

## Chat

### `POST /api/chat`

Start one agent run. The handler returns JSON immediately; output streams later through `/api/runs/{run_id}/stream`.

Request body (`core.ChatRequest` in `mycode-go/internal/core/service.go`):

```json
{
  "message": "...",
  "input": null,
  "session_id": "default",
  "provider": "anthropic",
  "model": "claude-sonnet-4-6",
  "cwd": "/path/to/current-dir",
  "api_key": null,
  "api_base": null,
  "reasoning_effort": "medium",
  "rewind_to": null
}
```

Exactly one of `message` or `input` is required.

- `provider` is either a configured provider alias or a raw provider id.
- `reasoning_effort` overrides config for this request only. Empty string, `null`, or `"auto"` means "use server/config default".
- `rewind_to` is a visible message index. It must point at a real user message, not an assistant message, synthetic compact summary, or tool-result-only message.

Structured `input` accepts the same block shape as Python:

```json
[
  {"type": "text", "text": "describe this"},
  {"type": "text", "text": "print('hi')", "name": "main.py", "is_attachment": true},
  {"type": "image", "path": "cat.png"},
  {"type": "image", "data": "<base64>", "mime_type": "image/png", "name": "cat.png"},
  {"type": "document", "path": "report.pdf"},
  {"type": "document", "data": "<base64>", "mime_type": "application/pdf", "name": "report.pdf"}
]
```

- Text attachments are wrapped as `<file name="...">...</file>` with `meta.attachment=true`.
- Image paths accept PNG, JPEG, GIF, and WebP.
- Documents currently accept PDF only.
- Inline `data` requires `mime_type`.
- The resolved model must support image input for `image` blocks and PDF input for `document` blocks.

Response:

```json
{
  "run": { "id": "...", "session_id": "...", "status": "running", "last_seq": 0 },
  "session": {
    "id": "...",
    "cwd": "...",
    "title": "...",
    "created_at": "...",
    "updated_at": "...",
    "message_format_version": 6
  }
}
```

Errors:

- `400` for invalid request data, invalid `rewind_to`, unsupported attachment type, unsupported model capability, or unsupported `reasoning_effort`.
- `409` when the session already has a running task. Body shape: `{"detail": {"message": "...", "run": {...}}}`.
- `500` for config/runtime failures other than invalid request values.

### `GET /api/runs/{run_id}/stream?after=0`

Stream run events as SSE (`text/event-stream`).

- `after` resumes from a sequence number.
- Each event is JSON encoded as one SSE `data:` line.
- Stream completion is `data: [DONE]`.
- Every event carries monotonically increasing `seq`.

### `POST /api/runs/{run_id}/cancel`

Cancel a running task. Returns:

```json
{"status": "ok", "run": {...}}
```

### `POST /api/runs/{run_id}/decide`

Resolve one pending tool permission request emitted by `permission_request`.

Request:

```json
{"request_id": "...", "decision": "allow"}
```

`decision` must be `"allow"` or `"deny"`. Returns `{"status": "ok"}`.

Errors:

- `400` for a missing `request_id` or invalid `decision`.
- `404` when the run or permission request is no longer pending.

### `GET /api/config?cwd=...`

Return provider, model, and capability metadata for the web UI.

```json
{
  "providers": {
    "<provider_name>": {
      "name": "...",
      "provider": "anthropic",
      "type": "anthropic",
      "models": ["claude-sonnet-4-6"],
      "base_url": "",
      "has_api_key": true,
      "supports_reasoning_effort": true,
      "reasoning_models": ["claude-sonnet-4-6"],
      "reasoning_effort": "auto",
      "supports_image_input": true,
      "image_input_models": ["claude-sonnet-4-6"],
      "supports_pdf_input": true,
      "pdf_input_models": ["claude-sonnet-4-6"]
    }
  },
  "default": { "provider": "<provider_name>", "model": "claude-sonnet-4-6" },
  "default_reasoning_effort": "auto",
  "reasoning_effort_options": ["auto", "none", "low", "medium", "high", "xhigh"],
  "cwd": "...",
  "config_paths": ["..."]
}
```

`GET /api/config` returns `503` when no provider can be resolved.

## Sessions

Session routes are implemented in `mycode-go/internal/server/sessions.go`.

### `GET /api/sessions?cwd=...`

List sessions. Optional `cwd` filters by the exact stored cwd. Each session includes `is_running`.

```json
{"sessions": [{"id": "...", "title": "...", "cwd": "...", "message_format_version": 6, "is_running": false}]}
```

### `POST /api/sessions`

Create a new empty session.

Request:

```json
{"cwd": null}
```

`cwd` defaults to the server's current working directory. The server allocates a uuid-like hex session id.

### `GET /api/sessions/{id}`

Load a session. If the session has an active run, the response overlays in-memory state:

```json
{
  "session": {...},
  "messages": [...],
  "active_run": {...},
  "pending_events": [...]
}
```

Missing sessions return `{"session": null, "messages": [], "active_run": null, "pending_events": []}`.

### `DELETE /api/sessions/{id}`

Delete a session directory. Returns `409` if the session has a running task.

### `POST /api/sessions/{id}/clear`

Truncate `messages.jsonl`, reset the title, and keep `meta.json`. Returns `409` if the session has a running task.

## Workspaces

Workspace routes are implemented in `mycode-go/internal/server/workspaces.go`.

### `GET /api/workspaces/roots`

List allowed workspace roots from `MYCODE_WORKSPACE_ROOTS` or `WORKSPACE_ROOTS` (comma-separated). Defaults to `$HOME` and `/`.

### `GET /api/workspaces/browse?root=...&path=...`

Browse directories within a root. Returns subdirectories only, skipping dotfiles.

```json
{
  "root": "/Users/example",
  "path": "projects",
  "current": "/Users/example/projects",
  "entries": [{"name": "mycode", "path": "projects/mycode"}],
  "error": ""
}
```

### `GET /api/workspaces/cwd`

Return the server process cwd:

```json
{"cwd": "...", "exists": true}
```

## SSE Contract

`GET /api/runs/{run_id}/stream` emits the same event names and payload fields as Python `main`.

| event                 | payload fields                                                               |
| --------------------- | ---------------------------------------------------------------------------- |
| `reasoning`           | `delta: str`                                                                 |
| `text`                | `delta: str`                                                                 |
| `tool_start`          | `tool_call: {id, name, input}`                                               |
| `tool_output`         | `tool_use_id: str`, `output: str`                                            |
| `tool_done`           | `tool_use_id: str`, `output: str`, `is_error: bool`, `metadata?`, `content?` |
| `compact`             | `message: str`                                                               |
| `error`               | `message: str`                                                               |
| `permission_request`  | `request_id: str`, `tool_use_id: str`, `tool_name: str`, `preview: str`      |
| `permission_resolved` | `request_id: str`, `decision: "allow" \| "deny"`                             |

The web UI replays `pending_events` from `GET /api/sessions/{id}`, then reconnects with `after=<last seq>`.

## Run Manager

`mycode-go/internal/core/run_manager.go` manages concurrent runs:

- One active run per session.
- Events are buffered for reconnect.
- Pending permission decisions are tracked per active run.
- Finished runs are pruned after 300 seconds.
- `snapshotSession()` returns base messages plus buffered events for active-run recovery.
