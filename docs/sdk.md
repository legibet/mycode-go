# Go SDK

The SDK packages expose the agent runtime without CLI/server policy.

- `agent` runs one agent loop.
- `message` defines the canonical message and block format.
- `attachment` converts file paths, bytes, and text snippets into message blocks.
- `tools` defines tool specs, tool calls, hooks, and built-in tools.
- `session` stores append-only JSONL sessions.
- `provider` defines provider adapters, requests, events, and built-in provider lookup.

App-only behavior stays outside the SDK: `.mycode` config loading, AGENTS discovery, system prompt construction, permission review UI, HTTP routes, and SSE framing.

## Agent Runtime

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/legibet/mycode-go/agent"
    "github.com/legibet/mycode-go/message"
    "github.com/legibet/mycode-go/tools"
)

func main() {
    ctx := context.Background()
    cwd, err := os.Getwd()
    if err != nil {
        panic(err)
    }

    a, err := agent.New(agent.Config{
        Provider:  "openai",
        Model:     "gpt-5",
        APIKey:    os.Getenv("OPENAI_API_KEY"),
        CWD:       cwd,
        System:    "You are a concise coding assistant.",
        ToolSpecs: tools.DefaultSpecs(),
    })
    if err != nil {
        panic(err)
    }

    user := message.UserTextMessage("Read README.md and summarize it.", nil)
    for event := range a.Chat(ctx, user, agent.ChatOptions{}) {
        if event.Type == "text" {
            fmt.Print(event.Data["delta"])
        }
        if event.Type == "error" {
            fmt.Fprintln(os.Stderr, event.Data["message"])
        }
    }
}
```

`a.Chat` runs one user turn and returns a channel of `agent.Event` values. The turn loops provider → tool calls → provider internally until the assistant stops calling tools, then the channel closes. Event types match the SSE contract in `docs/api.md`.

Config notes:

- `ToolSpecs` is explicit: leave it nil for a runtime with no tools, or pass `tools.DefaultSpecs()` to expose `read`, `write`, `edit`, and `bash`.
- `Provider` may be empty when the model id is recognizable (`claude-*`, `gpt-*`, `gemini-*`, …); `New` infers it via `provider.InferProviderFromModel` and errors when it can't.
- `Temperature` is an optional `*float64`: nil uses the provider default, otherwise the value (`0`–`1`) is sent.
- `CompactThreshold` defaults to `agent.DefaultCompactThreshold`; set `DisableCompact` to turn automatic compaction off.

## Attachments

`attachment.Build` returns canonical `message.Block` values.

```go
blocks := []message.Block{
    message.TextBlock("Describe these files.", nil),
}

attached, err := attachment.Build([]attachment.Attachment{
    attachment.Path("diagram.png"),
    attachment.Text("package main\n", "main.go"),
}, attachment.Options{CWD: cwd})
if err != nil {
    return err
}
blocks = append(blocks, attached...)

user := message.BuildMessage("user", blocks, nil)
```

`attachment.Build` resolves each item against `Options.CWD` and returns blocks in input order:

- A path to PNG/JPEG/GIF/WebP becomes an `image` block; a path to PDF becomes a `document` block.
- A path to a UTF-8 text file becomes a `<file name="…">…</file>` text block tagged with `meta.attachment`.
- `attachment.Bytes` carries raw `image/*` or `application/pdf` data without touching disk.
- `attachment.Text` wraps an inline snippet as the same `<file>` text block and requires a name.

A missing path, a directory, an unsupported binary, or an unsupported media type returns an error before the message is built.

## Sessions

Session persistence is opt-in. Use `session.Store` and pass `OnPersist`.

```go
store, err := session.NewStore("/tmp/mycode-sessions")
if err != nil {
    return err
}
sessionID := "example"

loaded, err := store.LoadSession(sessionID)
if err != nil {
    return err
}

var history []message.Message
if loaded != nil {
    history = loaded.Messages
}

a, err := agent.New(agent.Config{
    Provider:   "openai",
    Model:      "gpt-5",
    APIKey:     os.Getenv("OPENAI_API_KEY"),
    CWD:        cwd,
    SessionID:  sessionID,
    SessionDir: store.SessionDir(sessionID),
    Messages:   history,
})
if err != nil {
    return err
}

opts := agent.ChatOptions{
    OnPersist: func(msg message.Message) error {
        return store.AppendMessage(sessionID, msg, cwd)
    },
}

for event := range a.Chat(ctx, message.UserTextMessage("Continue.", nil), opts) {
    _ = event
}
```

## Custom Tools

Custom tools are plain `tools.ToolSpec` values.

```go
echo := tools.ToolSpec{
    Name:        "echo",
    Description: "Echo text.",
    InputSchema: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "text": map[string]any{"type": "string"},
        },
        "required":             []string{"text"},
        "additionalProperties": false,
    },
    Runner: func(ctx context.Context, call tools.ToolCall) tools.Result {
        text, _ := call.Input["text"].(string)
        return tools.Result{Output: text}
    },
}

a, err := agent.New(agent.Config{
    Provider:  "openai",
    Model:     "gpt-5",
    APIKey:    os.Getenv("OPENAI_API_KEY"),
    CWD:       cwd,
    ToolSpecs: []tools.ToolSpec{echo},
})
```

## Provider Adapters

Use a built-in provider by setting `Provider`, or pass a custom `provider.Adapter` in `agent.Config.Adapter`.

```go
type adapter struct{}

func (adapter) Spec() provider.Spec {
    return provider.Spec{ID: "local"}
}

func (adapter) StreamTurn(ctx context.Context, req provider.Request) <-chan provider.StreamEvent {
    out := make(chan provider.StreamEvent, 1)
    go func() {
        defer close(out)
        msg := message.BuildMessage("assistant", []message.Block{
            message.TextBlock("ok", nil),
        }, nil)
        out <- provider.StreamEvent{Type: "message_done", Msg: &msg}
    }()
    return out
}
```
