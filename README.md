# mycode-go

>There are many coding agents, but this one is mine.

A minimal coding agent.

- Minimal Go backend.
- Multiple provider support and robust message replay.
- 4 built-in tools (`read`, `write`, `edit`, `bash`), expanded via skills.
- Mobile-friendly web UI.
- Native image and pdf input support.

This branch is the Go CLI/server rewrite. It keeps the `.mycode` config and session layout, HTTP API, SSE events, provider ids, and web UI contract aligned with `mycode` on Python `main`. The terminal TUI is not part of this branch.

## Go SDK

The reusable agent runtime is exposed as small Go packages: `agent`, `message`, `attachment`, `tools`, `session`, and `provider`. CLI/server configuration, AGENTS discovery, system prompt construction, and permission UI stay in app code.

```go
a, err := agent.New(agent.Config{
    Provider: "openai",
    Model:    "gpt-5",
    APIKey:   os.Getenv("OPENAI_API_KEY"),
    CWD:      cwd,
    Tools:    []tools.Spec{tools.Read, tools.Write, tools.Edit, tools.Bash},
})
```

See [docs/sdk.md](docs/sdk.md) for the public interface and examples.

## Quick Start

Build a binary with the web UI embedded:

```bash
make build
```

Single message, non-interactive:

```bash
./dist/mycode-go run "explain how the session store works"
```

Resume the latest session for the current cwd:

```bash
./dist/mycode-go run --continue "continue the last task"
```

Web UI (default at `http://127.0.0.1:8000`):

```bash
./dist/mycode-go web [--port <port>] [--hostname <hostname>]
```

API keys are discovered automatically from environment variables (see Providers).

## Providers

| Provider          | id            | Env var                            |
| ----------------- | ------------- | ---------------------------------- |
| Anthropic         | `anthropic`   | `ANTHROPIC_API_KEY`                |
| OpenAI            | `openai`      | `OPENAI_API_KEY`                   |
| Google Gemini     | `google`      | `GEMINI_API_KEY`, `GOOGLE_API_KEY` |
| Moonshot          | `moonshotai`  | `MOONSHOT_API_KEY`                 |
| MiniMax           | `minimax`     | `MINIMAX_API_KEY`                  |
| DeepSeek          | `deepseek`    | `DEEPSEEK_API_KEY`                 |
| Z.AI              | `zai`         | `ZAI_API_KEY`                      |
| OpenRouter        | `openrouter`  | `OPENROUTER_API_KEY`               |
| OpenAI-compatible | `openai_chat` | -                                  |

## Configuration

A config file is optional. API keys from the environment are usually sufficient.

Create `~/.mycode/config.json` (global) or `.mycode/config.json` under the current project to:

- set a default provider, model, and reasoning effort
- expose additional models on an existing provider
- register a custom endpoint, such as a private or regional deployment
- set tool permission defaults

```json
{
  "default": {
    "provider": "anthropic",
    "model": "claude-sonnet-4-6",
    "reasoning_effort": "medium"
  },
  "permission": {
    "level": "safe",
    "mode": "ask"
  },
  "providers": {
    "openrouter": {
      "models": {
        "deepseek/deepseek-v3.2": {},
        "xiaomi/mimo-v2-pro": {}
      }
    },
    "zhipu-coding-plan": {
      "type": "zai",
      "base_url": "https://open.bigmodel.cn/api/coding/paas/v4",
      "api_key": "${ZHIPU_API_KEY}"
    },
    "custom-provider": {
      "type": "openai_chat",
      "base_url": "https://custom-endpoint.com/v1",
      "api_key": "${CUSTOM_API_KEY}",
      "models": {
        "custom-model": {
          "context_window": 128000,
          "max_output_tokens": 16384,
          "supports_reasoning": true,
          "supports_image_input": false,
          "supports_pdf_input": false
        }
      }
    }
  }
}
```

- To override a built-in provider, reuse its id as the key; custom providers must declare `type`.
- `reasoning_effort` controls extended thinking for supported models: `auto` (default) · `none` · `low` · `medium` · `high` · `xhigh`.
- `permission.level` controls automatic tool execution: `readonly` · `safe` · `standard` · `yolo`; default `safe`.
- `permission.mode` is `ask` or `deny`; non-interactive CLI runs treat `ask` as `deny`.
- API keys in config accept `${ENV_VAR}` references.
- Model metadata is bundled from [models.dev](https://models.dev); `{}` is enough for most models.

> Built-in Moonshot, MiniMax, and Z.AI providers default to international endpoints. Override `base_url` for China endpoints.

## CLI Reference

```bash
mycode-go "..."                      send one message, non-interactive
mycode-go run "..."                  send one message, non-interactive
mycode-go run --continue "..."       resume the most recent session for the current cwd
mycode-go run --session <id> "..."   resume a specific session
mycode-go web                        start web server (default port 8000)
mycode-go web --dev                  API only, no static files
mycode-go session list               list saved sessions for the current cwd
mycode-go session list --all         list saved sessions for all cwd values
```

## Development

```bash
git clone <repo> && cd mycode-go
make test
go run ./cmd/mycode-go "hello"
```

Web development (backend + Vite dev server):

```bash
make web-install
make dev
```

Other useful shortcuts: `make fmt` · `make lint` · `make check` · `make build`

Refresh the bundled model catalog:

```bash
uv run --no-project python ./scripts/update_models_catalog.py
```

## License

MIT
