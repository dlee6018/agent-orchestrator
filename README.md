# go-orchestrator

A Go CLI tool that orchestrates coding agent sessions via tmux. It supports two modes:

- **Chat mode** (`AUTONOMOUS_MODE=false`): Human-in-the-loop — sends user input as keystrokes to a tmux pane running the coding agent, polls until output stabilizes, and prints results back.
- **Autonomous mode** (`AUTONOMOUS_MODE=true`, default): An LLM (via OpenRouter) replaces the human — it receives a task, drives the coding agent back and forth, and stops when done.

The inner coding agent is selected via `DEFAULT_MODEL`: models starting with `gpt` use Codex, all others default to Claude Code. `CLAUDE_CMD` can override the command entirely.

Both modes automatically recover from tmux session or server crashes.

## Prerequisites

- Go 1.23+
- [tmux](https://github.com/tmux/tmux) installed and on PATH
- [Claude CLI](https://docs.anthropic.com/en/docs/claude-code) or [Codex](https://github.com/openai/codex) installed and on PATH
- An [OpenRouter](https://openrouter.ai/) API key (autonomous mode only)

## Usage

```bash
go build -o go-orchestrator .

# Autonomous mode (default) — prompts for a task description:
OPENROUTER_API_KEY=<key> ./go-orchestrator

# Optionally specify a working directory:
OPENROUTER_API_KEY=<key> ./go-orchestrator /path/to/project

# Use Codex as the inner agent:
DEFAULT_MODEL=gpt-4o OPENROUTER_API_KEY=<key> ./go-orchestrator

# Chat mode — interactive prompt:
AUTONOMOUS_MODE=false ./go-orchestrator
```

In chat mode, type a message and press Enter to send it to the agent. Type `/quit` to exit.

In autonomous mode, enter a task description when prompted. The orchestrator LLM will drive the coding agent until it signals `TASK_COMPLETE`.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `CLAUDE_TMUX_SESSION` | `gt-claude-loop` | tmux session name |
| `CLAUDE_TMUX_SOCKET` | `gt-claude-loop` | tmux socket name (isolates from user's tmux) |
| `DEFAULT_MODEL` | `claude` | Selects the inner coding agent (`gpt*` → Codex, otherwise → Claude Code) |
| `CLAUDE_CMD` | (derived from `DEFAULT_MODEL`) | Overrides the command to run inside the tmux session |
| `TERMINATE_WHEN_QUIT` | `false` | Kill the tmux session on `/quit` or signal (SIGINT/SIGTERM) |
| `AUTONOMOUS_MODE` | `true` | Agent loop when true; interactive chat when false |
| `OPENROUTER_API_KEY` | (required in autonomous mode) | OpenRouter API key |
| `OPENROUTER_MODEL` | `anthropic/claude-opus-4.6` | Model for the orchestrator LLM |
| `MAX_ITERATIONS` | `0` (unlimited) | Safety cap on agent loop iterations |
| `DASHBOARD_ENABLED` | `true` | Enable/disable the web dashboard |
| `DASHBOARD_PORT` | `0` (auto) | Port for the dashboard (0 = OS picks a free port) |
| `DASHBOARD_OPEN` | `true` | Auto-open browser when dashboard starts |
| `MEMORY_MAX_FACTS` | `50` | Threshold for triggering memory compaction |

## Testing

Unit tests (no tmux required):

```bash
go test -v ./...
```

Integration tests (requires tmux):

```bash
go test -v -tags=integration ./...
```

Integration tests create isolated tmux servers via unique sockets and clean up after themselves. They mutate package-level globals so they must NOT use `t.Parallel()`.

## Architecture

The project is split into focused packages with no external dependencies (stdlib only):

| Package | Description |
|---|---|
| `main` (root) | Entry point, `runWithCleanup()`, `chatLoop()`, default constants |
| `helpers/` | Environment and config utilities — `LoadEnvFile`, `EnvOrDefault`, `EnvBool`, `ValidateSessionName`, `ResolveAgentConfig` |
| `tmux/` | Tmux session management and I/O — session lifecycle, message sending, pane polling, text cleaning |
| `dashboard/` | SSE broker + embedded web dashboard (`dashboard/web/`) |
| `orchestrator/` | Autonomous loop + OpenRouter API — `AutonomousLoop`, `CallOpenRouter`, `BuildSystemPrompt`, API types |
| `memory/` | Persistent memory — load/save `memory.json`, extract `MEMORY_SAVE:` lines, deduplication, compaction |

### Dependency graph (acyclic)

```
helpers  (no deps)
tmux     (no deps)
dashboard (no deps)
memory   (no deps — uses CompactFunc callback)
orchestrator → tmux, memory, dashboard
main → helpers, tmux, dashboard, memory, orchestrator
```

### Persistent memory

The orchestrator LLM can emit `MEMORY_SAVE: <fact>` lines in its replies. These are extracted, deduplicated, and saved to `memory.json` in the working directory when the autonomous loop exits. On the next run, saved facts are loaded and injected into the system prompt. When the fact count exceeds `MEMORY_MAX_FACTS`, an LLM-based compaction step consolidates them.
