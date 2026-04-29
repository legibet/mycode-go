# mycode-go — Agent Context

Authoritative context for agent runs. Keep this file short and in sync with the Go branch.

## Product

`mycode-go` is a personal minimal coding agent with a web UI and a small CLI. It keeps `.mycode` config and session compatibility with the original Python version.

This branch is the Go rewrite of the Python backend on `main`. It tracks Python `main` for external behavior, but it is not a line-by-line port:

- `web/` is synced directly from Python `main` by cherry-picking web-only commits.
- Go backend code mirrors external behavior, disk formats, and API contracts from Python `main` (`cli/` and `mycode/`).
- Go backend internals should stay idiomatic Go. Do not copy Python package layout or implementation details unless it simplifies compatibility.

## Current Sync

Synced with Python `main` through:

- `1635719 Release 0.7.5`

Repository branch model (three-tier, see `docs/branching.md`):

- `main` — Python implementation, primary development branch, source of truth for web/ and external contracts.
- `mycode-go` (**this branch**) — Go rewrite with HTTP, web UI, and a small non-interactive CLI. Tracks `main`. Internal `core` service layer is transport-agnostic so the desktop branch can reuse it.
- `mycode-go-wails` — adds a Wails desktop adapter on top of `mycode-go`. Tracks `mycode-go`. Wails toolchain, `main.go`/`app.go`, and the `web/utils/transport.ts` abstraction live only there.

Priorities:

- small readable core
- one message model
- one agent loop
- append-only sessions
- provider adapters at the boundary
- Python-compatible message/session/API contracts

## Core Rules

- 4 built-in tools only: `read`, `write`, `edit`, `bash`
- Provider-specific behavior stays inside adapters
- Keep the runtime explicit and easy to inspect
- Prefer simple Go over framework-heavy designs
- Do not add abstraction layers unless they remove real complexity
- New session writes use the current Python-compatible format only; do not add legacy session compatibility paths unless explicitly requested
- Keep `web/` byte-for-byte aligned with Python `main` unless a Go-branch-only web patch is explicitly documented

## Source Map

Core runtime:

- `mycode-go/internal/core/*.go` — transport-agnostic service layer (chat/sessions/runs/config/workspace) shared by HTTP and future adapters; owns `RunManager`
- `mycode-go/internal/agent/agent.go` — the only orchestration loop
- `mycode-go/internal/message/message.go` — canonical message/block format
- `mycode-go/internal/tools/*.go` — 4 built-in tools and execution
- `mycode-go/internal/permissions/*.go` — CLI/web tool permission policy and conservative bash classification
- `mycode-go/internal/session/store.go` — append-only JSONL storage, compact, rewind, repair
- `mycode-go/internal/config/*.go` — layered config loading and provider resolution
- `mycode-go/internal/models/catalog.go` — bundled model metadata lookup
- `mycode-go/internal/prompt/prompt.go` — runtime system prompt assembly, AGENTS discovery, skills discovery

Providers:

- `mycode-go/internal/provider/base.go` — adapter contract and replay helpers
- `mycode-go/internal/provider/registry.go` — adapter registry
- `mycode-go/internal/provider/specs.go` — built-in provider metadata
- `mycode-go/internal/provider/anthropic.go` — `anthropic`, `moonshotai`, `minimax`
- `mycode-go/internal/provider/openai_responses.go` — `openai`
- `mycode-go/internal/provider/openai_chat.go` — `openai_chat`, `deepseek`, `zai`, `openrouter`
- `mycode-go/internal/provider/google.go` — `google`

Server:

- `mycode-go/internal/server/app.go` + `mycode-go/internal/server/*.go` — thin HTTP adapter over `internal/core.Service`; owns SSE framing and request parsing only
- `mycode-go/internal/workspace/workspace.go` — workspace browser

CLI:

- `mycode-go/cmd/mycode-go/*.go` — `run`, `web`, `session list`, `--version`, and bare-message convenience mode

Web UI:

- `web/src/hooks/useChat.ts` — chat state and SSE streaming
- `web/src/utils/messages.ts` — canonical blocks to UI messages

## Internal Message Model

All runtime, persistence, and API data use the same block-based JSON format:

```json
{
  "role": "assistant",
  "content": [
    {"type": "thinking", "text": "..."},
    {"type": "text", "text": "..."},
    {"type": "tool_use", "id": "call_1", "name": "read", "input": {"path": "x.go"}},
    {"type": "tool_result", "tool_use_id": "call_1", "output": "...", "metadata": {}, "is_error": false}
  ],
  "meta": {
    "provider": "anthropic",
    "model": "claude-sonnet-4-6",
    "stop_reason": "tool_use",
    "total_tokens": 1456,
    "context_window": 200000,
    "native": {}
  }
}
```

Block types:

- `text`
- `image`
- `document`
- `thinking`
- `tool_use`
- `tool_result`

Tool results are stored as a `user` message with `tool_result` blocks. `thinking` blocks are first-class session data.

`tool_result.output` is provider-facing text. `tool_result.metadata` is structured UI data and is optional. Do not write `model_text` or `display_text`.

Session `meta.json` stores only `cwd`, `title`, `created_at`, `updated_at`, and `message_format_version=6`. The API adds `id` from the directory name. Provider/model/api_base live on per-turn messages, not in session meta.

## Agent Loop

`mycode-go/internal/agent/agent.go` runs one user turn:

1. Append user message
2. Stream one provider turn
3. Persist the assistant message
4. Execute tool calls locally
5. Append tool results as a `user` message
6. Repeat until there are no tool calls
7. Optionally compact context when usage crosses `compact_threshold`

## Provider Types

See `docs/providers.md` for details. All provider ids are preserved:

- `anthropic`
- `moonshotai`
- `minimax`
- `openai`
- `openai_chat`
- `deepseek`
- `zai`
- `openrouter`
- `google`

## SSE Contract

Do not change these event names or shapes without updating server and web UI.

- `reasoning` — `delta`
- `reasoning_done` — `duration_ms`
- `text` — `delta`
- `tool_start` — `tool_call: {id, name, input}`
- `tool_output` — `tool_use_id`, `output`
- `tool_done` — `tool_use_id`, `output`, `is_error`, optional `metadata`, optional `content`
- `compact` — `message`
- `error` — `message`
- `permission_request` — `request_id`, `tool_use_id`, `tool_name`, `preview`
- `permission_resolved` — `request_id`, `decision` (`"allow"` or `"deny"`)
- `usage` — `total_tokens`, optional `model`, optional `provider`, optional `context_window`

Every event also carries `seq`.

## Interfaces

CLI:

- `mycode-go <message>` — convenience alias for one non-interactive run
- `mycode-go run "..."` — one non-interactive run
- `mycode-go web [--dev]` — web server
- `mycode-go session list` — list sessions

This Go rewrite does not include terminal TUI.

Server:

- `POST /api/chat`
- `GET /api/runs/{run_id}/stream`
- `POST /api/runs/{run_id}/cancel`
- `POST /api/runs/{run_id}/decide`
- `GET /api/config`
- `GET /api/settings`
- `PUT /api/settings`
- session CRUD at `/api/sessions`
- workspace browser at `/api/workspaces`

## Commit Conventions

`web/` changes and backend changes must be in **separate commits**.

Commit message format: `type(scope): description`

Scopes:

- `web` — changes under `web/` only
- `backend` — Go backend changes only
- `cli` — CLI changes only

When syncing web changes from `main`:

```bash
# fetch Python main into this repository
git fetch /Users/legibet/projects/mycode main:refs/remotes/local-main/main

# find web commits since last sync
git log --reverse --oneline <last-sync>..refs/remotes/local-main/main -- web/

# cherry-pick a specific web commit
git cherry-pick <hash>

# verify web is fully synced
git diff --stat refs/remotes/local-main/main -- web
```

Backend sync rules:

- Compare Python `main` behavior after the recorded sync commit.
- Directly cherry-pick `web/` commits.
- Reimplement backend commits in Go when they affect CLI behavior, API/SSE contracts, message/session formats, tools, providers, config, models, prompts, or web expectations.
- Skip Python-only release/package/TUI work unless it changes shared behavior.
- Update `docs/` and this file when contracts or sync status change.

## Dev Workflow

Backend:

```bash
go -C mycode-go test ./...
go -C mycode-go vet ./...
cd mycode-go && golangci-lint run ./...
go -C mycode-go run ./cmd/mycode-go web --dev
uv run --no-project python ./scripts/update_models_catalog.py # only supported way to update models_catalog.json
```

Web:

```bash
pnpm --dir web install
pnpm --dir web typecheck
pnpm --dir web test:run
pnpm --dir web dev
pnpm --dir web build
./scripts/sync_web_dist.sh
```

Compatibility smoke:

```bash
# Python writes a v6 session and Go must read it.
# Go writes a v6 session and Python must read it.
# Check tool_result.output and edit metadata patch/stats both ways.
```

## Guardrails

Preserve unless explicitly asked to change:

- 4-tool core stays unchanged
- append-only sessions stay human-inspectable
- CLI and server stay thin wrappers over `mycode-go/internal/agent`
- provider quirks stay in adapters
- no unnecessary abstraction layers
