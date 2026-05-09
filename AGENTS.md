# mycode-go — Agent Context

Always-loaded context for agent runs on this branch. Detailed specs live in `docs/`; this file points at them rather than duplicating their content.

## Product

`mycode-go` is the Go rewrite of `mycode`: a minimal coding agent with a small CLI, HTTP server, and shared React web UI. It keeps `.mycode` config, message, session, API, and SSE compatibility with the Python `main` branch.

Branches:

- `main` — Python implementation; source of truth for `web/`, message/session formats, HTTP API, and SSE contracts.
- `mycode-go` — this Go rewrite; tracks `main`, keeps Go internals idiomatic, and stays free of Wails code.
- `mycode-go-wails` — Wails desktop adapter on top of `mycode-go`; owns desktop entry points and build files.

Sync direction: `main` → `mycode-go` → `mycode-go-wails`.

Current sync: Python `main` through `d441ed1 refactor(web): reduce unnecessary effects`. Web commits through that point were cherry-picked directly. Python SDK/CLI refactors in this range were reviewed and do not require Go behavior changes. `2724b4c Release 0.8.4` only updates Python package metadata and `uv.lock`, so it does not apply here. `models_catalog.json` was regenerated from the shared update script.

Priorities: small readable core · one message model · one agent loop · append-only sessions · provider adapters at the boundary · Python-compatible contracts.

## Guardrails

- 4 built-in tools only: `read`, `write`, `edit`, `bash`.
- Provider-specific behavior stays inside adapters, never in the agent loop.
- Sessions stay append-only and human-inspectable.
- CLI and server stay thin wrappers over `mycode-go/internal/agent` and `internal/core`.
- Keep `web/` byte-for-byte aligned with Python `main` unless a Go-branch-only web patch is explicitly documented.

When in doubt, prefer the simpler and more explicit design.

## Project Layout

```text
mycode-go/
  cmd/mycode-go/              # CLI: run, web, session list, --version
  internal/agent/             # agent loop and compact replay
  internal/core/              # transport-agnostic service layer and RunManager
  internal/message/           # canonical block-based message model
  internal/tools/             # the 4 built-in tools and execution
  internal/permissions/       # CLI/web tool permission policy
  internal/session/           # append-only JSONL store, compact, rewind, repair
  internal/config/            # layered config loading and provider resolution
  internal/models/            # bundled model metadata lookup
  internal/prompt/            # system prompt, AGENTS discovery, skills discovery
  internal/provider/          # provider adapters
    anthropic.go              # anthropic, moonshotai, minimax
    google.go                 # google
    openai_responses.go       # openai
    openai_chat.go            # openai_chat, deepseek, zai, openrouter
  internal/server/            # HTTP adapter and SSE framing
  internal/workspace/         # workspace browser

web/src/                      # shared React + Vite UI from Python main
  hooks/useChat.ts            # chat state + SSE streaming
  utils/messages.ts           # canonical blocks → UI messages

scripts/
  update_models_catalog.py    # regenerates mycode-go/internal/models/models_catalog.json
  sync_web_dist.sh            # copies web/dist into Go's embedded webdist directory
```

## Internal Message Model

A single block-based JSON format is used at runtime, in persistence, and over the API. Block types: `text` · `image` · `document` · `thinking` · `tool_use` · `tool_result`.

Tool results are stored as `user` messages whose `tool_result` blocks carry provider-facing `output` plus optional structured UI `metadata`. `thinking` blocks are first-class session data. Session `meta.json` stores only `cwd`, `title`, `created_at`, `updated_at`, and `message_format_version=7`; provider/model/api_base live on per-turn messages.

Full schema, JSONL record types, replay rules, compact, and rewind behavior live in `docs/sessions.md`.

## Agent Loop

Per user turn (`mycode-go/internal/agent/agent.go`):

1. Append the user message.
2. Stream one provider turn through `ApplyCompactReplay`.
3. Persist the assistant message.
4. Execute local tool calls.
5. Append a `user` tool-result message.
6. Repeat until there are no tool calls.
7. Optionally compact when usage crosses `compact_threshold`; the compact event is persisted inline and projected on the next provider call.

## Provider Adapters

Adapter ids: `anthropic`, `moonshotai`, `minimax`, `google`, `openai`, `openai_chat`, `deepseek`, `zai`, `openrouter`. Provider wire-format quirks, reasoning replay, image/PDF serialization, and env vars live in `docs/providers.md`.

## SSE Contract

`GET /api/runs/{run_id}/stream` event types: `reasoning`, `reasoning_done`, `text`, `tool_start`, `tool_output`, `tool_done`, `compact`, `error`, `permission_request`, `permission_resolved`, `usage`. Every event carries a monotonically increasing `seq`.

Event names and payload shapes are a cross-component contract. Changes need to land in server and web UI together. Full payload fields and reconnect semantics live in `docs/api.md`.

## Detailed Specs

Read the relevant doc before related changes.

| Area                                                                 | Doc                                           |
| -------------------------------------------------------------------- | --------------------------------------------- |
| `internal/{agent,message,tools}`                                     | `docs/sdk.md`                                 |
| `internal/session` or anything touching JSONL / compact / rewind     | `docs/sessions.md`                            |
| `internal/provider/*`                                                | `docs/providers.md`                           |
| `internal/core`, `internal/server`, SSE events, or routes            | `docs/api.md`                                 |
| `internal/config`, `internal/prompt`, `internal/permissions`, models | `docs/config.md`                              |
| `web/src/**`                                                         | `docs/web.md`                                 |
| Cross-cutting contract changes                                       | `docs/api.md` + `docs/sdk.md` + `docs/web.md` |

For third-party SDKs and APIs touched by adapter or runtime code, prefer `context7` lookups over assumptions.

## Interfaces

CLI commands: `mycode-go <message>`, `mycode-go run "..."`, `mycode-go web [--dev]`, `mycode-go session list`. This Go rewrite does not include the Python terminal TUI.

Server routes are mounted under `/api`: chat runs, run stream/cancel/decide, config, settings, sessions, and workspaces. Endpoint schemas and run lifecycle details live in `docs/api.md`.

## Sync Rules

When syncing from Python `main`:

- Fetch `main` into `refs/remotes/local-main/main`.
- Directly cherry-pick `web/` commits.
- Reimplement backend commits in Go only when they affect CLI behavior, API/SSE contracts, message/session formats, tools, providers, config, models, prompts, or web expectations.
- Skip Python-only release/package/TUI work unless it changes shared behavior.
- Keep `web/` commits separate from Go backend, config, model, and docs commits.

Useful checks:

```bash
git log --reverse --oneline <last-sync>..refs/remotes/local-main/main -- web/
git diff --stat refs/remotes/local-main/main -- web
```

## Commit Conventions

Format: `type(scope): description`.

Scopes:

- `web` — changes under `web/` only
- `backend` — Go backend changes only
- `cli` — CLI changes only
- `docs` — documentation only

When a sync needs both web and backend/docs/model changes, make separate commits.

## Dev Workflow

```bash
go -C mycode-go test ./...
go -C mycode-go vet ./...
cd mycode-go && golangci-lint run ./...
go -C mycode-go run ./cmd/mycode-go web --dev

pnpm --dir web install
pnpm --dir web typecheck
pnpm --dir web test:run
pnpm --dir web dev
pnpm --dir web build
./scripts/sync_web_dist.sh
```

Update bundled model metadata with:

```bash
uv run --no-project python ./scripts/update_models_catalog.py
```

Compatibility smoke:

```text
Python writes a v7 session and Go must read it.
Go writes a v7 session and Python must read it.
Check tool_result.output and edit metadata patch/stats both ways.
```
