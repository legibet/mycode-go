# Branch Model

This repository has three long-lived branches, each with a single, clear job.

```text
main                      Python implementation (primary, source of truth)
  └─▶ mycode-go           Go rewrite, HTTP + web UI only
        └─▶ mycode-go-wails   Adds Wails desktop adapter
```

Each descendant tracks its direct parent. The `main` branch is never aware of `mycode-go`; the `mycode-go` branch is never aware of `mycode-go-wails`.

## Per-branch responsibilities

### `main` — Python

- Primary development branch.
- Authoritative for: message/session formats, HTTP API, SSE contract, `web/` source.
- Never contains Go code or Wails artifacts.

### `mycode-go` — Go rewrite (HTTP-only)

- Go port of the Python backend. Tracks `main`.
- Contains a full `mycode-go/` Go tree plus a synchronized `web/`.
- Splits the Go code into two layers:
  - `internal/core/` — transport-agnostic service (chat/sessions/runs/config/workspace). Takes an `EventSink` for streaming. Safe to call from any adapter.
  - `internal/server/` — thin HTTP/SSE adapter over `core.Service`.
- Ships a CLI (`mycode-go run`, `mycode-go web`, `mycode-go session list`).
- Does **not** depend on Wails. `go.mod` stays free of `wails/v2`.

### `mycode-go-wails` — Desktop adapter

- Adds a Wails desktop app on top of `mycode-go`. Tracks `mycode-go`.
- Introduces only:
  - `mycode-go/main.go`, `mycode-go/app.go`, `mycode-go/wails.json`
  - `mycode-go/frontend/dist/` embed hole (populated by build script; `.gitkeep` is tracked so `go:embed` compiles)
  - `scripts/build_wails_frontend.sh`, `scripts/build_wails_app.sh`
  - `web/src/utils/transport.ts` plus the runtime switch in `web/src/hooks/useChat.ts`, `App.tsx`, `WorkspacePicker.tsx`, `types.ts`
  - Wails indirect deps in `go.mod` / `go.sum`
  - `docs/wails.md`
- Does not modify files under `mycode-go/internal/core/` or `mycode-go/internal/server/` — those come unchanged from the base branch.

## Sync workflow

### Python `main` → `mycode-go`

```bash
# Fetch the Python repository as a named remote (one-time, or refresh)
git fetch /Users/legibet/projects/mycode main:refs/remotes/local-main/main

# Inspect pending web/ commits since last sync
git log --reverse --oneline <last-sync>..refs/remotes/local-main/main -- web/

# Cherry-pick each web commit as-is
git cherry-pick <hash>

# Backend changes are re-implemented idiomatically in Go (not cherry-picked)
# — compare Python behavior after <last-sync> and match external contracts.

# After syncing, web/ should be byte-aligned:
git diff --stat refs/remotes/local-main/main -- web
```

### `mycode-go` → `mycode-go-wails`

The Wails branch rebases onto `mycode-go`:

```bash
git checkout mycode-go-wails
git rebase mycode-go
```

Conflict hot spots (always the same files):

- `web/src/hooks/useChat.ts` — `fetch(...)` in main becomes `transport.xxx(...)` here. Port each new fetch call.
- `web/src/App.tsx`, `web/src/components/WorkspacePicker.tsx`, `web/src/types.ts` — minor runtime-mode branching.

Backend changes inside `internal/core/` and `internal/server/` should rebase cleanly; if they don't, the Wails branch is incorrectly diverging and should be re-aligned.

## Rules

- **No Wails code on `mycode-go`**. `main.go`/`app.go`/`wails.json`/`frontend/` belong only on `mycode-go-wails`.
- **No Python code on `mycode-go*`**. The Go branches never touch the `mycode/` Python tree.
- **`web/` stays byte-aligned with Python `main` on `mycode-go`**. Any divergence on `mycode-go-wails` must be listed under "Wails-only files" above.
- **`web/` and backend changes live in separate commits**, scoped `web(...)`, `backend(...)`, or `cli(...)`.

## Building the desktop app

Only on `mycode-go-wails`:

```bash
git checkout mycode-go-wails
./scripts/build_wails_app.sh
# → mycode-go/build/bin/mycode.app
```

See `docs/wails.md` for the development loop (`wails dev`) and ad-hoc codesign details.
