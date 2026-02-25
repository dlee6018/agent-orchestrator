# go-orchestrator

## What This Is

A Go CLI that orchestrates coding agent sessions via tmux. It supports three modes:
- **Chat mode** (`AUTONOMOUS_MODE=false`): Human-in-the-loop — reads stdin, sends messages to the coding agent, prints output.
- **Autonomous mode** (`AUTONOMOUS_MODE=true`, default): An LLM (via OpenRouter) replaces the human — it receives a task, drives the coding agent back and forth, and stops when done.
- **Multi-agent mode** (`MULTI_AGENT_MODE=true`): Three agent sessions (planner, executor, verifier) coordinated by the orchestrator LLM via JSON directives. Agents run concurrently with async message queuing.

The inner coding agent is selected via `DEFAULT_MODEL`: models starting with `gpt` use Codex, all others default to Claude Code. `CLAUDE_CMD` can override the command entirely. The agent display name (e.g. "Claude Code", "Codex") is propagated through `AutonomousLoop` and `BuildSystemPrompt` so prompts and logs are agent-aware.

## Package Structure

| Package | Description |
|---|---|
| `main` (root) | Entry point, `runWithCleanup()`, `chatLoop()`, default constants |
| `helpers/` | Environment and config utilities — `LoadEnvFile`, `EnvOrDefault`, `EnvBool`, `ValidateSessionName`, `ResolveAgentConfig` |
| `tmux/` | Tmux session management and I/O — session lifecycle, message sending, pane polling, text cleaning |
| `dashboard/` | SSE broker + embedded web dashboard (`dashboard/web/`) |
| `orchestrator/` | Autonomous loop + OpenRouter API — `AutonomousLoop`, `MultiAgentLoop`, `CallOpenRouter`, `BuildSystemPrompt`, `BuildMultiAgentSystemPrompt`, API types |
| `memory/` | Persistent memory — load/save `memory.json`, extract `MEMORY_SAVE:` lines, deduplication, compaction |
| `config/` | Interactive `/setup` wizard and persistent `config.json` management — `ConfigDir`, `LoadConfig`, `SaveConfig`, `ApplyConfig`, `RunSetupWizard` |

### Test Files

| File / Package | Covers |
|---|---|
| `helpers/helpers_test.go` | `LoadEnvFile`, `EnvOrDefault`, `EnvBool`, `ValidateSessionName`, `ResolveAgentConfig` |
| `tmux/tmux_test.go` | Tmux arg building, command resolution, pane state parsing, pane update polling, ANSI cleaning, truncation |
| `dashboard/dashboard_test.go` | SSE broker pub/sub, replay, unsubscribe, slow clients, event JSON, dashboard HTTP serving |
| `orchestrator/orchestrator_test.go` | System prompt building, OpenRouter API client (mock server tests), multi-agent directive parsing, agent lookup, multi-agent prompt, context summarization |
| `memory/memory_test.go` | Memory load/save round-trip, `ExtractMemorySaves`, deduplication, compaction |
| `config/config_test.go` | Config load/save round-trip, ApplyConfig env vars, RunSetupWizard (happy path, invalid input, EOF) |
| `main_test.go` (root) | `ResolveAgentConfig` integration with main, `CLAUDE_CMD` override logic |
| `integration_test.go` (root) | All cross-package integration tests: real tmux session lifecycle, autonomous loop with mock OpenRouter, SSE events, persistent memory, multi-agent loop (routing, queuing, recovery, SSE) |

### Other Files

- `dashboard/web/` — Embedded web dashboard assets (compiled into the binary via `//go:embed`)
- `go.mod` — Go 1.23, module `github.com/dlee6018/agent-orchestrator`, no external dependencies
- `.env` — Runtime env vars
- `~/.config/go-orchestrator/config.json` — Persistent user settings from `/setup` wizard (global, not per-project; highest precedence, overrides `.env`)
- `README.md` — Project readme

## Dependency Graph (acyclic)

```
helpers  (no deps)
tmux     (no deps)
dashboard (no deps)
memory   (no deps — uses CompactFunc callback)
config   (no deps)
orchestrator → tmux, memory, dashboard
main → helpers, tmux, dashboard, memory, orchestrator, config
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `CLAUDE_TMUX_SESSION` | `gt-claude-loop` | tmux session name |
| `CLAUDE_TMUX_SOCKET` | `gt-claude-loop` | tmux socket name |
| `DEFAULT_MODEL` | `claude` | Selects the inner coding agent (`gpt*` → Codex, otherwise → Claude Code) |
| `CLAUDE_CMD` | (derived from `DEFAULT_MODEL`) | Overrides the command to run inside tmux |
| `AUTONOMOUS_MODE` | `true` | Agent loop when true; existing chatLoop when false |
| `OPENROUTER_API_KEY` | (required in autonomous mode) | OpenRouter API key |
| `OPENROUTER_MODEL` | `anthropic/claude-opus-4.6` | Model for the orchestrator LLM |
| `MAX_ITERATIONS` | `0` (unlimited) | Safety cap on agent loop iterations |
| `DASHBOARD_ENABLED` | `true` | Enable/disable the web dashboard |
| `DASHBOARD_PORT` | `0` (auto) | Port for the dashboard (0 = OS picks a free port) |
| `DASHBOARD_OPEN` | `true` | Auto-open browser when dashboard starts |
| `MEMORY_MAX_FACTS` | `50` | Threshold for triggering memory compaction |
| `MULTI_AGENT_MODE` | `false` | Enable 3-agent orchestration (planner, executor, verifier) |
| `CONTEXT_SUMMARY_THRESHOLD` | `30` | Message count before older context is summarized (multi-agent mode) |
| `CONTEXT_KEEP_RECENT` | `10` | Number of recent messages to preserve when summarizing context (multi-agent mode) |

## Build & Run

```bash
go build -o go-orchestrator .

# Autonomous mode (default):
OPENROUTER_API_KEY=<key> ./go-orchestrator

# Use Codex as the inner agent:
DEFAULT_MODEL=gpt-4o OPENROUTER_API_KEY=<key> ./go-orchestrator

# Multi-agent mode:
MULTI_AGENT_MODE=true OPENROUTER_API_KEY=<key> ./go-orchestrator

# Chat mode:
AUTONOMOUS_MODE=false ./go-orchestrator
```

## Testing

Unit tests (no tmux needed):
```bash
go test -v ./...
```

Integration tests (requires tmux on PATH):
```bash
go test -v -tags=integration ./...
```

Integration tests create isolated tmux servers via unique sockets and clean themselves up. They mutate package-level globals so they must NOT use `t.Parallel()`.

## Conventions

- Make small, focused changelists — each commit should do one logical thing (e.g., add a function, fix a bug, refactor a single concern). Avoid large, sweeping commits that mix unrelated changes. Even inside a large change, there should be multiple commits
- Commit often — after completing each small, self-contained unit of work, commit immediately with a clear message. Do not batch multiple unrelated changes into a single commit.
- After making changes, always provide a summary explaining what was changed and why
- Add a comment above every function describing what it does
- When writing tests, cover both success and failure cases — not just the happy path
- When fixing a bug or broken functionality, always add a regression test that reproduces the issue and verifies the fix — this prevents the same problem from recurring
- Multi-package architecture — each domain (tmux, dashboard, orchestrator, memory, helpers) in its own package under the project root
- No external dependencies — stdlib only
- Error messages include the function name as prefix (e.g., `"createSession: new-session: ..."`)
- Input validation uses allowlists (e.g., `ValidateSessionName` permits only `[a-zA-Z0-9_-]`)
- Package-level vars (`PollInterval`, `StableWindow`, `StartupSettleWindow`, `Socket`, `Endpoint`, `MaxIterations`, `MaxFacts`, `KeystrokeSleep`, `ContextSummaryThreshold`) are overridden in tests
- The autonomous loop completion signal is the literal string `TASK_COMPLETE` checked via `strings.Contains`
- Circular dependencies are broken via callbacks (e.g., `memory.CompactFunc` avoids memory → orchestrator dependency)
- The inner agent command and display name are resolved via `helpers.ResolveAgentConfig` and threaded through `AutonomousLoop`/`MultiAgentLoop` and `BuildSystemPrompt`/`BuildMultiAgentSystemPrompt`
- Multi-agent mode uses JSON directives (`{"agent":"...","message":"..."}`) for LLM→agent routing
- Multi-agent mode supports async message queuing (max 1 per agent) when targeting busy agents
- Dashboard `IterationEvent` has optional `Agent` and `Mode` fields (omitempty — backward compatible with single-agent mode)
