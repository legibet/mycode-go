# Web UI

`web/` is React + Vite. Its source must stay byte-for-byte synced with Python main's `web/` directory.

## Sync Rule

Do not manually port web changes. Sync by cherry-picking main-branch web commits:

```bash
git fetch /Users/legibet/projects/mycode main:refs/remotes/local-main/main
git log --reverse --oneline <last-sync>..refs/remotes/local-main/main -- web/
git cherry-pick <web-commit>
```

After syncing, verify:

```bash
git diff --stat refs/remotes/local-main/main -- web
```

Expected output is empty, except when the Go branch intentionally carries a documented web-only patch.

## Serving Modes

- `mycode-go web` serves `web/dist` in local development when available, otherwise embedded assets from `mycode-go/internal/server/webdist`.
- `mycode-go web --dev` serves API only; pair it with `pnpm --dir web dev`.
- `MYCODE_WEB_DIST` can point the server at a custom built asset directory.

Built assets are synced into the Go embed directory with:

```bash
pnpm --dir web build
./scripts/sync_web_dist.sh
```

## Source Layout

```text
web/src/
  App.tsx
  main.tsx
  types.ts
  components/
    Chat/
      InputArea.tsx
      MessageBubble.tsx
      MessageList.tsx
      ToolCard.tsx
      EditDiff.tsx
      PermissionPrompt.tsx
      ReasoningBlock.tsx
      MarkdownBlock.tsx
      CodeBlock.tsx
    Sidebar.tsx
    WorkspacePicker.tsx
    MobileHeader.tsx
    ThemeProvider.tsx
  hooks/
    useChat.ts
    sessionSelection.ts
  utils/
    messages.ts
    storage.ts
    config.ts
    highlighter.ts
```

## Message Rendering Contract

The web consumes canonical session messages and SSE events:

- `tool_use` blocks render as `ToolCard`.
- Persisted `tool_result` user messages are folded into the preceding assistant message.
- Live tool state is tracked in `ToolRuntime`.
- `tool_done.output` is the final display/provider text.
- `tool_done.metadata` carries structured UI payloads such as edit `patch` and line stats.
- `permission_request` shows the approval panel; `permission_resolved` clears it, including after reconnect.

The reducer keeps:

- `rawMessages` — canonical block messages
- `messages` — render-ready messages
- `toolRuntimeById` — live per-tool state

State updates are immutable; keep reducers simple and deterministic.

## Streaming Flow

1. `POST /api/chat` returns `{run, session}`.
2. `GET /api/runs/{run_id}/stream` streams SSE.
3. The reducer applies each event.
4. On disconnect, the client reloads `GET /api/sessions/{id}`.
5. If a run is still active, the client applies `pending_events` and reconnects with `after=<last_seq>`.
6. `409` conflict attaches to the existing active run.
7. Tool approval decisions are sent with `POST /api/runs/{run_id}/decide`.

## Local Storage

The web persists:

- provider
- model
- cwd
- reasoningEffort
- active session per cwd
- cwd history

Empty reasoning effort and `auto` both mean "do not send a per-request effort override".

## Verification

```bash
pnpm --dir web typecheck
pnpm --dir web test:run
pnpm --dir web build
```

The Go server should work with the same `web/` source as Python main. Backend differences must be handled in Go API compatibility, not in web forks.
