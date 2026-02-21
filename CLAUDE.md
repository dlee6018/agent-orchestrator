# go-orchestrator

## What This Is

A Go CLI that orchestrates Claude Code sessions via tmux. It supports two modes:
- **Chat mode** (`AUTONOMOUS_MODE=false`): Human-in-the-loop — reads stdin, sends messages to Claude Code, prints output.
- **Autonomous mode** (`AUTONOMOUS_MODE=true`, default): An LLM (via OpenRouter) replaces the human — it receives a task, drives Claude Code back and forth, and stops when done.

## Package Structure

| Package | Description |
|---|---|
| `main` (root) | Entry point, `runWithCleanup()`, `chatLoop()`, default constants |
| `helpers/` | Environment and config utilities — `LoadEnvFile`, `EnvOrDefault`, `EnvBool`, `ValidateSessionName` |
| `tmux/` | Tmux session management and I/O — session lifecycle, message sending, pane polling, text cleaning |
| `dashboard/` | SSE broker + embedded web dashboard (`dashboard/web/`) |
| `orchestrator/` | Autonomous loop + OpenRouter API — `AutonomousLoop`, `CallOpenRouter`, `BuildSystemPrompt`, API types |
| `memory/` | Persistent memory — load/save, extract MEMORY_SAVE lines, deduplication, compaction |

### Test Files

| File / Package | Covers |
|---|---|
| `helpers/helpers_test.go` | `LoadEnvFile`, `EnvOrDefault`, `EnvBool`, `ValidateSessionName` |
| `tmux/tmux_test.go` | Tmux arg building, command resolution, pane state parsing, pane update polling, ANSI cleaning, truncation |
| `dashboard/dashboard_test.go` | SSE broker pub/sub, replay, unsubscribe, slow clients, event JSON, dashboard HTTP serving |
| `orchestrator/orchestrator_test.go` | System prompt building, OpenRouter API client (mock server tests) |
| `memory/memory_test.go` | Memory load/save round-trip, `ExtractMemorySaves`, deduplication, compaction |
| `integration_test.go` (root) | All cross-package integration tests: real tmux session lifecycle, autonomous loop with mock OpenRouter, SSE events, persistent memory |

### Other Files

- `dashboard/web/` — Embedded web dashboard assets (compiled into the binary via `//go:embed`)
- `go.mod` — Go 1.23, module `github.com/dlee6018/agent-orchestrator`, no external dependencies
- `.env` — Runtime env vars (contains `TERMINATE_WHEN_QUIT=true`)
- `README.md` — Project readme

## Dependency Graph (acyclic)

```
helpers  (no deps)
tmux     (no deps)
dashboard (no deps)
memory   (no deps — uses CompactFunc callback)
orchestrator → tmux, memory, dashboard
main → helpers, tmux, dashboard, memory, orchestrator
```

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `CLAUDE_TMUX_SESSION` | `gt-claude-loop` | tmux session name |
| `CLAUDE_TMUX_SOCKET` | `gt-claude-loop` | tmux socket name |
| `CLAUDE_CMD` | `claude --dangerously-skip-permissions --setting-sources user` | Command to run inside tmux |
| `TERMINATE_WHEN_QUIT` | `false` | Kill tmux session on `/quit` or signal |
| `AUTONOMOUS_MODE` | `true` | Agent loop when true; existing chatLoop when false |
| `OPENROUTER_API_KEY` | (required in autonomous mode) | OpenRouter API key |
| `OPENROUTER_MODEL` | `anthropic/claude-opus-4.6` | Model for the orchestrator LLM |
| `MAX_ITERATIONS` | `0` (unlimited) | Safety cap on agent loop iterations |
| `DASHBOARD_ENABLED` | `true` | Enable/disable the web dashboard |
| `DASHBOARD_PORT` | `0` (auto) | Port for the dashboard (0 = OS picks a free port) |
| `DASHBOARD_OPEN` | `true` | Auto-open browser when dashboard starts |
| `MEMORY_MAX_FACTS` | `50` | Threshold for triggering memory compaction |

## Build & Run

```bash
go build -o go-orchestrator .

# Autonomous mode (default):
OPENROUTER_API_KEY=<key> ./go-orchestrator

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

- After making changes, always provide a summary explaining what was changed and why
- Add a comment above every function describing what it does
- When writing tests, cover both success and failure cases — not just the happy path
- Multi-package architecture — each domain (tmux, dashboard, orchestrator, memory, helpers) in its own package under the project root
- No external dependencies — stdlib only
- Error messages include the function name as prefix (e.g., `"createSession: new-session: ..."`)
- Input validation uses allowlists (e.g., `ValidateSessionName` permits only `[a-zA-Z0-9_-]`)
- Package-level vars (`PollInterval`, `StableWindow`, `StartupSettleWindow`, `Socket`, `Endpoint`, `MaxIterations`, `MaxFacts`, `KeystrokeSleep`) are overridden in tests
- The autonomous loop completion signal is the literal string `TASK_COMPLETE` checked via `strings.Contains`
- Circular dependencies are broken via callbacks (e.g., `memory.CompactFunc` avoids memory → orchestrator dependency)
