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

`ToolSpecs` is explicit. Leave it nil for a runtime with no tools. Pass `tools.DefaultSpecs()` to expose `read`, `write`, `edit`, and `bash`.

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

Supported media path and byte attachments are PNG, JPEG, GIF, WebP, and PDF. Other UTF-8 files become named `<file>` text blocks. Other binary files return an error.

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
