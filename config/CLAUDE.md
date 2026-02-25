# config

## Purpose

Interactive setup wizard and persistent config file (`config.json`) management. No cross-package dependencies (stdlib only). Follows the same JSON load/save pattern as `memory/memory.go`.

## Exported API

### Constants

| Symbol | Description |
|---|---|
| `FileName` | `"config.json"` — the persistent config file name |

### Types

| Type | Description |
|---|---|
| `Config` | Struct with string fields mapping to env vars: `DefaultModel`, `OpenRouterModel`, `DashboardEnabled`, `MultiAgentMode`, `AutonomousMode` |

### Functions

| Function | Description |
|---|---|
| `ConfigDir()` | Returns the global config directory path (`~/.config/go-orchestrator`) |
| `LoadConfig(dir)` | Reads config.json from dir, returns parsed Config (nil if file missing) |
| `SaveConfig(dir, cfg)` | Writes Config as pretty-printed JSON to config.json (creates dir if needed) |
| `ApplyConfig(cfg)` | Sets env vars via `os.Setenv` for each non-empty field |
| `RunSetupWizard(scanner, w)` | Interactive numbered-choice wizard (5 questions), returns completed Config |

## Key Implementation Details

- All Config fields are strings (not bools) because they map directly to env var values consumed by `helpers.EnvBool`/`helpers.EnvOrDefault`
- `ApplyConfig` unconditionally overwrites env vars (giving config.json highest precedence)
- `ApplyConfig` skips fields that are empty strings
- `RunSetupWizard` takes a `*bufio.Scanner` (not `io.Reader`) to avoid the two-scanners-on-same-stdin problem
- Invalid wizard input (out of range, non-numeric, empty) re-prompts; EOF returns an error
- `ConfigDir()` resolves to `~/.config/go-orchestrator` via `os.UserHomeDir()` — global, not per-project
- `SaveConfig` creates the config directory via `os.MkdirAll` if it doesn't exist (first `/setup` run)
- `LoadConfig` and `SaveConfig` keep their `dir string` parameter for testability (tests pass `t.TempDir()`)
- Precedence: `config.json` (ApplyConfig) > `.env` (LoadEnvFile skips already-set vars) > defaults (EnvOrDefault)

## Testing

```bash
go test -v ./config/
```

Tests use `t.TempDir()`, `strings.NewReader`, `bufio.NewScanner`, and `bytes.Buffer`. No external dependencies.
