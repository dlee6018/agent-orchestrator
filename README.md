# go-orchestrator

A Go CLI tool that orchestrates Claude Code sessions via tmux. It supports two modes:

- **Chat mode** (`AUTONOMOUS_MODE=false`): Human-in-the-loop — sends user input as keystrokes to a tmux pane running Claude CLI, polls until output stabilizes, and prints results back.
- **Autonomous mode** (`AUTONOMOUS_MODE=true`, default): An LLM (via OpenRouter) replaces the human — it receives a task, drives Claude Code back and forth, and stops when done.

Both modes automatically recover from tmux session or server crashes.

## Prerequisites

- Go 1.23+
- [tmux](https://github.com/tmux/tmux) installed and on PATH
- [Claude CLI](https://docs.anthropic.com/en/docs/claude-code) installed and on PATH
- An [OpenRouter](https://openrouter.ai/) API key (autonomous mode only)

## Usage

```bash
go build -o go-orchestrator .

# Autonomous mode (default) — prompts for a task description:
OPENROUTER_API_KEY=<key> ./go-orchestrator

# Optionally specify a working directory:
OPENROUTER_API_KEY=<key> ./go-orchestrator /path/to/project

# Chat mode — interactive prompt:
AUTONOMOUS_MODE=false ./go-orchestrator
```

In chat mode, type a message and press Enter to send it to Claude. Type `/quit` to exit.

In autonomous mode, enter a task description when prompted. The orchestrator LLM will drive Claude Code until it signals `TASK_COMPLETE`.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `CLAUDE_TMUX_SESSION` | `gt-claude-loop` | tmux session name |
| `CLAUDE_TMUX_SOCKET` | `gt-claude-loop` | tmux socket name (isolates from user's tmux) |
| `CLAUDE_CMD` | `claude --dangerously-skip-permissions --setting-sources user` | Command to run inside the tmux session |
| `TERMINATE_WHEN_QUIT` | `false` | Kill the tmux session on `/quit` or signal (SIGINT/SIGTERM) |
| `AUTONOMOUS_MODE` | `true` | Agent loop when true; interactive chat when false |
| `OPENROUTER_API_KEY` | (required in autonomous mode) | OpenRouter API key |
| `OPENROUTER_MODEL` | `anthropic/claude-opus-4.6` | Model for the orchestrator LLM |
| `MAX_ITERATIONS` | `0` (unlimited) | Safety cap on agent loop iterations |

## Testing

Unit tests (no tmux required):

```bash
go test -v ./...
```

Integration tests (requires tmux):

```bash
go test -v -tags=integration ./...
```

Integration tests create isolated tmux servers via unique sockets and clean up after themselves.

## Architecture

The entire application lives in a single file (`main.go`) with these key components:

- **`main` / `runWithCleanup`** — Entry point, config resolution, signal handling, session cleanup
- **`chatLoop`** — Interactive read-eval-print loop (chat mode)
- **`autonomousLoop`** — LLM agent loop: calls OpenRouter → sends reply to Claude Code → captures output → feeds back to LLM → repeats until `TASK_COMPLETE` (autonomous mode)
- **`callOpenRouter`** — Sends chat completion requests to the OpenRouter API
- **`ensureClaudeSession`** — Creates or validates the tmux session, restarts if the pane is dead
- **`sendAndCaptureWithRecovery`** — Sends a message, waits for output, retries once on recoverable failures
- **`waitForPaneUpdate`** — Polls `capture-pane` until output changes and stabilizes for a configurable window
- **`waitForRuntimeReady`** — Blocks until the tmux session is alive and the startup command hasn't crashed
- **`restartClaudeSession`** — Kills and recreates the session from scratch
- **`cleanPaneOutput`** — Strips ANSI escapes, collapses blank lines, trims whitespace from pane captures
