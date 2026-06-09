# Go SDK

The SDK packages expose the agent runtime without CLI/server policy.

- `agent` runs one agent loop.
- `message` defines the canonical message and block format.
- `attachment` converts file paths, bytes, and text snippets into message blocks.
- `tools` defines the tool `Spec`, `Define` for custom tools, hooks, and the built-in `Read`/`Write`/`Edit`/`Bash` tools.
- `session` stores append-only JSONL sessions.
- `provider` defines provider requests, events, and built-in provider lookup.

App-only behavior stays outside the SDK: `.mycode` config loading, AGENTS discovery, system prompt construction, permission review UI, HTTP routes, and SSE framing.

## Agent Runtime

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/legibet/mycode-go/agent"
    "github.com/legibet/mycode-go/tools"
)

func main() {
    ctx := context.Background()

    a, err := agent.New(agent.Config{
        Provider: "openai",
        Model:    "gpt-5",
        APIKey:   os.Getenv("OPENAI_API_KEY"),
        System:   "You are a concise coding assistant.",
        Tools:    []tools.Spec{tools.Read, tools.Write, tools.Edit, tools.Bash},
    })
    if err != nil {
        panic(err)
    }

    for event := range a.Chat(ctx, "Read README.md and summarize it.") {
        if event.Type == "text" {
            fmt.Print(event.Data["delta"])
        }
        if event.Type == "error" {
            fmt.Fprintln(os.Stderr, event.Data["message"])
        }
    }
}
```

`a.Chat` takes a prompt string (plus optional attachments) and returns a channel of `agent.Event` values. The turn loops provider → tool calls → provider internally until the assistant stops calling tools, then the channel closes. Event types match the SSE contract in `docs/api.md`. Use `a.ChatMessage` to pass a fully built `message.Message` (e.g. multi-modal content).

Config notes:

- `Tools` is explicit: leave it nil for a runtime with no tools, or list the built-in `tools.Read`, `tools.Write`, `tools.Edit`, `tools.Bash` values (any subset) together with custom tools.
- `Provider` may be empty when the model id is recognizable (`claude-*`, `gpt-*`, `gemini-*`, …); `New` infers it via `provider.InferProviderFromModel` and errors when it can't.
- Model capabilities resolve from the bundled catalog: leave `MaxTokens`/`ContextWindow` zero and `SupportsImageInput`/`SupportsPDFInput` nil to use the catalog values (falling back to `16384`/`128000`), or set them to override. `New` resolves them via `provider.ResolveModel`.
- `Temperature` is an optional `*float64`: nil uses the provider default, otherwise the value (`0`–`1`) is sent.
- `CompactThreshold` defaults to `agent.DefaultCompactThreshold`; set `DisableCompact` to turn automatic compaction off.

## Attachments

Pass attachments straight to `Chat`. They are resolved against the agent's CWD
and appended to the user message in order.

```go
for event := range a.Chat(ctx, "Describe these files.",
    attachment.Path("diagram.png"),
    attachment.Text("package main\n", "main.go"),
) {
    _ = event
}
```

Each item becomes a canonical block:

- A path to PNG/JPEG/GIF/WebP becomes an `image` block; a path to PDF becomes a `document` block.
- A path to a UTF-8 text file becomes a `<file name="…">…</file>` text block tagged with `meta.attachment`.
- `attachment.Bytes` carries raw `image/*` or `application/pdf` data without touching disk.
- `attachment.Text` wraps an inline snippet as the same `<file>` text block and requires a name.

A missing path, a directory, an unsupported binary, or an unsupported media type surfaces as an `error` event before the provider is called. For full control, build blocks with `attachment.Build` and pass a `message.Message` to `ChatMessage`.

## Sessions

Session persistence is opt-in: pass a `*session.Store` and a `SessionID`. The
agent resumes history from disk when the session exists, then appends every
message it produces — user input, assistant replies, tool results, and compact
markers — on its own.

```go
store, err := session.NewStore("/tmp/mycode-sessions")
if err != nil {
    return err
}

a, err := agent.New(agent.Config{
    Provider:  "openai",
    Model:     "gpt-5",
    APIKey:    os.Getenv("OPENAI_API_KEY"),
    CWD:       cwd,
    Store:     store,
    SessionID: "example",
})
if err != nil {
    return err
}

for event := range a.Chat(ctx, "Continue.") {
    _ = event
}
```

Leave `Store` nil to keep the run in memory. With a `Store`, leave `Messages`
nil to resume an existing session. Passing `Messages` is accepted only for a new
session id, so an existing transcript cannot be mixed with a different history.

## Custom Tools

Build a custom tool with `tools.Define`. The input type's fields drive the
provider schema by reflection — the `json` tag names the wire key, `omitempty`
marks an optional field, and `jsonschema_description` carries the description.
The decoded, typed input arrives in `Call.Input`.

```go
type echoArgs struct {
    Text string `json:"text" jsonschema_description:"Text to echo."`
}

echo := tools.Define("echo", "Echo text.",
    func(ctx context.Context, c tools.Call[echoArgs]) tools.Result {
        return tools.Result{Output: c.Input.Text}
    })

a, err := agent.New(agent.Config{
    Provider: "openai",
    Model:    "gpt-5",
    APIKey:   os.Getenv("OPENAI_API_KEY"),
    CWD:      cwd,
    Tools:    []tools.Spec{tools.Bash, echo},
})
```

Built-in and custom tools share one `[]tools.Spec`. Pass `tools.WithStreaming()`
to `Define` for a tool that emits incremental output through `Call.Emit`.
