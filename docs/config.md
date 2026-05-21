# Configuration

Source: `mycode-go/internal/config/`

## Config Files

Loaded in order, later values overriding earlier values:

1. `~/.mycode/config.json` — global
2. `.mycode/config.json` files from `project` to `cwd`

`project` is the nearest parent directory containing `.git`. When no `.git` is found, `project` is `cwd`.

Explicit request args (CLI flags, API params) override both.

Config resolution: `config.Load(cwd)` returns `Settings`.

The web UI's settings panel edits only the global file. Project-level files continue to override it. Settings API validation lives in `mycode-go/internal/config`; runtime request handling lives in `mycode-go/internal/core/service.go`.

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

### Fields

- `default.provider` — references a key in `providers`, or a raw adapter id
- `default.model` — model name used when no per-provider model is selected
- `default.reasoning_effort` — global default; `null`/`"auto"`/`"default"` all resolve to no override
- `default.compact_threshold` — fraction of context window that triggers compaction; `false` or `0` disables; range `[0, 1]`; default `0.8`
- `permission` — tool execution permissions. String shorthand (`"safe"`) sets the level and keeps the current/default mode; object form accepts `level` and `mode`
- `permission.level` — how much the agent may run automatically: `readonly` · `safe` · `standard` · `yolo`; default `safe`
- `permission.mode` — what to do outside the selected level: `ask` or `deny`; default `ask`. Non-interactive `mycode-go run` treats `ask` as `deny`
- `providers.<name>.type` — internal adapter id. Required for custom aliases. Built-in providers can omit `type` when the key matches their adapter id.
- `providers.<name>.models` — model map. Keys are model ids shown in UI. Values can override bundled model metadata for that exact model.
- `providers.<name>.models.<model>.context_window` — override the model context window
- `providers.<name>.models.<model>.max_output_tokens` — override the provider output limit
- `providers.<name>.models.<model>.supports_reasoning` — override whether reasoning effort is available
- `providers.<name>.models.<model>.supports_image_input` — override image input support
- `providers.<name>.models.<model>.supports_pdf_input` — override PDF input support
- `providers.<name>.api_key` — literal value or `${ENV_NAME}` reference
- `providers.<name>.base_url` — override the adapter's default base URL
- `providers.<name>.reasoning_effort` — per-provider override of the global default

## API Key Resolution Order

For a resolved provider (`resolveProviderRuntime` in `config.go`):

1. Explicit `api_key` param from CLI/API.
2. Config `api_key`.
   - `${ENV_NAME}` — dereferenced from env at resolution time
   - plain string — used as-is
3. Provider adapter's built-in default env vars, such as `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`.

If no API key is found at any step, provider resolution returns an error listing which env vars were checked.

## Provider Resolution

`config.ResolveProvider(settings, providerName, model, apiKey, apiBase)` returns a `ResolvedProvider`:

1. If `providerName` is given: resolve it as a configured alias or raw provider id; failures raise.
2. If no `providerName`: try the configured default; failures fall through to step 3.
3. Iterate configured providers with valid credentials, then env-discoverable built-in providers.
4. If nothing is found: return an error listing checked env vars.

Auto-discovery is limited to providers where `AutoDiscoverable` is true and the corresponding env var is set.

Configured provider order and model order preserve JSON object order.

## Reasoning Effort

Controls how much thinking a model does.

Config resolution: `providers.<name>.reasoning_effort` -> `default.reasoning_effort`.

Request override: `POST /api/chat` normalizes `reasoning_effort` and passes it through directly when set.

Options: `auto` (default) · `none` · `low` · `medium` · `high` · `xhigh`

- `auto` — do not send any effort parameter; let the provider decide
- `none` — explicitly disable thinking
- `off` and `disabled` normalize to `none`
- Config-derived effort is applied only when `Spec.SupportsReasoningEffort` and catalog `supports_reasoning` are both true
- Invalid request values are rejected by `POST /api/chat` with `400`
- See `docs/providers.md` for per-adapter mapping details

## Tool Permissions

CLI and web server agents classify tool calls before execution. The web UI prompts for approval when `mode: "ask"` and the call falls outside the configured `level`. Non-interactive `mycode-go run` has no prompt, so it treats `ask` as `deny` and returns the denial to the model as the tool result.

Levels:

- `readonly` — automatically allow clear read-only actions under `project`, discovered skill reads, and simple read-only shell commands (`ls`, `rg`, `git status`, `git diff`, etc.)
- `safe` — `readonly` plus `project`-local `write`/`edit`. Shell commands remain limited to clear read-only commands.
- `standard` — `safe` plus ordinary single shell commands, unless they match dangerous or compound-command checks.
- `yolo` — automatically allow all tool calls.

Mode:

- `ask` — prompt for approval in the web UI.
- `deny` — reject without prompting.

Automatic denials do not stop the run; the model receives the denied tool result and can reply with next steps. An explicit web `deny` cancels the current run.

The shell checks are intentionally simple and conservative. Project commands such as tests, builds, formatters, package scripts, and task runners are `standard` because they execute project-defined code. Compound commands (`&&`, `||`, `;`, pipes, redirection, command substitution) and obvious destructive commands (`rm`, `sudo`, `chmod`, `git reset`, `git clean`, `git push --force`, etc.) fall outside `readonly`/`safe`/`standard` and require `yolo` or `mode: "ask"` approval.

## Model Metadata

`mycode-go/internal/models/catalog.go` reads the bundled `mycode-go/internal/models/models_catalog.json` catalog to look up:

- `supports_reasoning` — whether the model supports extended thinking
- `supports_image_input` — whether the model accepts image input
- `supports_pdf_input` — whether the model accepts PDF input
- `context_window` — used for compact threshold calculation; defaults to `128000` when not available
- `max_output_tokens` — passed to the provider as the output limit; defaults to `16384` when not available

When the catalog has no match and config does not override the capability, media and reasoning support stay disabled: image/PDF input is rejected, and `reasoning_effort` is only sent when `supports_reasoning` is explicitly `true` and the provider adapter supports it.

Model lookup strategy:

1. Exact match on the given provider type + raw model id.
2. Fallback provider mapping, such as `claude-*` -> `anthropic`, `deepseek-*` -> `deepseek`.
3. OpenRouter catalog suffix fallback (`provider/model` matched by `model`) as last resort.

The bundled catalog is updated by running:

```bash
uv run --no-project python ./scripts/update_models_catalog.py
```

## Skills Discovery

`mycode-go/internal/prompt/prompt.go` scans for `SKILL.md` files and injects an `<available_skills>` block into the system prompt.

Scan roots, lowest to highest priority:

1. `~/.agents/skills/` — compatibility global root
2. `~/.mycode/skills/` — global root
3. `.agents/skills/` from `project` to `cwd` — compatibility project roots
4. `.mycode/skills/` from `project` to `cwd` — project roots

Each `SKILL.md` requires YAML frontmatter with `name` and `description`. Later roots override earlier ones by skill name. Max scan depth: 3 directory levels, max 200 directories per root.

The model uses the `read` tool to load full skill content on demand from the skill `path`.

## Instructions Discovery

`mycode-go/internal/prompt/prompt.go` reads `AGENTS.md` files and injects them as `<project_instructions>` into the system prompt. Files checked:

1. `~/.mycode/AGENTS.md` (fallback: `~/.agents/AGENTS.md`)
2. all `AGENTS.md` files from `project` to `cwd`

Later files are more specific and take precedence.

## Project Boundary

`cwd` is the current working directory. `project` is the nearest parent directory containing `.git`; when no `.git` is found, `project` is `cwd`.

Config, instructions, and skill discovery walk from `project` to `cwd`, so nearer files have higher priority. Tool permissions treat paths inside `project` as project-local and require approval for paths outside `project`.

## Sessions Directory

`config.ResolveSessionsDir()` returns `$MYCODE_HOME/sessions`, defaulting to `~/.mycode/sessions`. See `docs/sessions.md`.

## Port

Server port: `PORT` env var -> config default `8000`, overridden by `mycode-go web --port`.
