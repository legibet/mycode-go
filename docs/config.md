# Configuration

Source: `mycode-go/internal/config/`

The config format and resolution order match the Python CLI under `main`'s `cli/` tree. The Go implementation keeps the logic in Go structs and functions.

## Config Files

Loaded in order, later values overriding earlier values:

1. `~/.mycode/config.json`
2. `.mycode/config.json` files from `project` to `cwd`

`project` is the nearest parent directory containing `.git`. When no `.git` is found, `project` is `cwd`.

Explicit CLI flags or API request fields override both config files.

## Schema

```json
{
  "default": {
    "provider": "anthropic",
    "model": "claude-sonnet-4-6",
    "reasoning_effort": "auto",
    "compact_threshold": 0.8
  },
  "permission": {
    "level": "safe",
    "mode": "ask"
  },
  "providers": {
    "<name>": {
      "type": "<adapter-id>",
      "models": {
        "model-a": {
          "context_window": 400000,
          "max_output_tokens": 128000,
          "supports_reasoning": true,
          "supports_image_input": true,
          "supports_pdf_input": true
        },
        "model-b": {}
      },
      "base_url": "https://...",
      "api_key": "sk-..." or "${ENV_VAR_NAME}",
      "reasoning_effort": "none"
    }
  }
}
```

## Fields

- `default.provider` references a key in `providers`, or a raw built-in provider id.
- `default.model` is used when no provider model is selected.
- `default.reasoning_effort` accepts `auto`, `none`, `low`, `medium`, `high`, `xhigh`, plus `default`/empty as aliases for `auto`.
- `default.compact_threshold` is a fraction in `[0, 1]`; `false` or `0` disables compaction.
- `permission` controls automatic tool execution. String shorthand such as `"safe"` sets the level and keeps the current/default mode.
- `permission.level` accepts `readonly`, `safe`, `standard`, or `yolo`; default `safe`.
- `permission.mode` accepts `ask` or `deny`; default `ask`. Non-interactive `mycode-go run` treats `ask` as `deny`.
- `providers.<name>.type` is required for custom aliases. Built-in providers can omit it when the provider key matches the adapter id.
- `providers.<name>.models` controls UI model order and per-model capability overrides.
- `providers.<name>.api_key` is either a literal key or `${ENV_NAME}`.
- `providers.<name>.base_url` overrides the adapter default base URL.
- `providers.<name>.reasoning_effort` overrides the global default for that provider.

## API Key Resolution

For one resolved provider:

1. Explicit `api_key` from CLI/API.
2. Config `api_key`, including `${ENV_NAME}` indirection.
3. Built-in provider env vars.

Provider resolution fails when no usable key is found. The error lists the checked env vars.

## Provider Resolution

`config.ResolveProvider(settings, providerName, model, apiKey, apiBase, reasoningEffort)` returns a runnable `ResolvedProvider`.

Resolution order:

1. Requested provider alias or raw provider id.
2. Configured default provider.
3. Configured providers with usable credentials, in JSON order.
4. Env-discoverable built-in providers with usable credentials, in registry order.

Configured provider order and model order preserve JSON object order, matching Python.

## Reasoning Effort

Options: `auto`, `none`, `low`, `medium`, `high`, `xhigh`.

- `auto` means no explicit provider effort parameter.
- `none` explicitly disables thinking when supported.
- `off` and `disabled` normalize to `none`.
- Config-derived effort is applied only when both the adapter and model support reasoning.
- Invalid request values are rejected by `POST /api/chat` with `400`.

Provider-specific mapping is documented in `docs/providers.md`.

## Tool Permissions

The Go branch implements the Python web/CLI permission behavior directly in Go. It does not expose a general hooks system.

Levels:

- `readonly` allows `project`-local `read`, discovered skill reads, and simple read-only shell commands such as `ls`, `rg`, `git status`, and `git diff`.
- `safe` adds `project`-local `write` and `edit`.
- `standard` adds ordinary single shell commands unless they are dangerous or compound.
- `yolo` allows all tool calls.

Modes:

- `ask` prompts in the web UI for calls outside the configured level. In non-interactive CLI runs, `ask` is handled as `deny`.
- `deny` returns `error: permission denied` to the model without prompting.

The bash classifier is conservative. Compound commands, shell redirection, command substitution, destructive programs, `sed -i`, dangerous `find` flags, `git reset`, `git clean`, `git checkout`, `git restore`, and force pushes require `yolo` or web approval.

## Model Metadata

`mycode-go/internal/models/models_catalog.json` is copied from Python `main`'s `mycode/src/mycode/models_catalog.json` and looked up through `mycode-go/internal/models/catalog.go`.

Metadata fields:

- `supports_reasoning`
- `supports_image_input`
- `supports_pdf_input`
- `context_window`
- `max_output_tokens`

Lookup strategy:

1. Exact provider + model.
2. Fallback provider mapping for common model prefixes.
3. Unique OpenRouter catalog suffix fallback (`provider/model` matched by `model`).

Update the catalog with:

```bash
uv run --no-project python ./scripts/update_models_catalog.py
```

## Prompt Context

`mycode-go/internal/prompt/prompt.go` builds the runtime system prompt.

It includes:

- base mycode instructions
- `<project_instructions>` from AGENTS files
- `<available_skills>` from discovered skills
- current working directory
- current date as `YYYY-MM`

Instruction files checked:

1. `~/.mycode/AGENTS.md`, falling back to `~/.agents/AGENTS.md`
2. all `AGENTS.md` files from `project` to `cwd`

Skill roots, lowest to highest priority:

1. `~/.agents/skills/`
2. `~/.mycode/skills/`
3. `.agents/skills/` from `project` to `cwd`
4. `.mycode/skills/` from `project` to `cwd`

Each `SKILL.md` requires YAML frontmatter with `name` and `description`. Later roots override earlier roots by skill name.

## Sessions Directory

`config.ResolveSessionsDir()` returns `$MYCODE_HOME/sessions`, defaulting to `~/.mycode/sessions`.

Python's SDK layer makes persistence opt-in. This Go branch is the CLI/server backend, so it resolves the session directory by default.

## Port

Server port: `PORT` env var → config default `8000`, overridden by `mycode-go web --port`.
