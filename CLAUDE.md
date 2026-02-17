# go-orchestrator

## What This Is

A Go CLI that orchestrates Claude Code sessions via tmux. It supports two modes:
- **Chat mode** (`AUTONOMOUS_MODE=false`): Human-in-the-loop — reads stdin, sends messages to Claude Code, prints output.
- **Autonomous mode** (`AUTONOMOUS_MODE=true`, default): An LLM (via OpenRouter) replaces the human — it receives a task, drives Claude Code back and forth, and stops when done.

## Project Structure

- `main.go` — Entire application in a single file (entry point, tmux management, chat loop, autonomous loop, OpenRouter client, recovery logic)
- `main_test.go` — Unit tests (no tmux required)
- `main_integration_test.go` — Integration tests (requires tmux, uses `//go:build integration` tag)
- `go.mod` — Go 1.23, module `github.com/dlee6018/agent-orchestrator`, no external dependencies
- `.env` — Runtime env vars (contains `TERMINATE_WHEN_QUIT=true`)

## Key Architecture

All code lives in `main.go` in a single `package main`. There are no subdirectories or packages.

### Core Flow
1. `main()` → resolves config from env vars → `ensureClaudeSession()` → branch on `AUTONOMOUS_MODE`
2. **Chat mode**: `chatLoop()` reads stdin, calls `sendAndCaptureWithRecovery()` per message
3. **Autonomous mode**: `autonomousLoop()` calls OpenRouter LLM, sends its replies to Claude Code, feeds pane output back, repeats until `TASK_COMPLETE`
4. `sendAndCaptureWithRecovery()` ensures session → sends keystrokes → polls for stable output → retries once on recoverable failures
5. `waitForPaneUpdate()` polls `capture-pane` until output changes and stabilizes (configurable `stableWindow`)

### Key Functions
- `ensureClaudeSession` — Creates or validates the tmux session, restarts if dead
- `sendAndCaptureWithRecovery` — Send + capture with one retry on recoverable errors
- `waitForPaneUpdate` / `waitForPaneUpdateWithCapture` — Poll-based output detection
- `waitForRuntimeReady` — Blocks until tmux session is alive and startup command hasn't crashed
- `restartClaudeSession` — Kill + recreate session from scratch
- `resolveStartupCommand` — Validates command, resolves binary to absolute path via `LookPath`
- `shouldRecoverSession` — Checks error strings to decide if session restart is warranted
- `autonomousLoop` — Agent loop: LLM decides → send to Claude Code → capture output → feed back to LLM → repeat
- `callOpenRouter` — Sends chat completion request to OpenRouter API, returns reply and token usage
- `cleanPaneOutput` — Strips ANSI escapes, collapses blank lines, trims whitespace from pane captures
- `buildSystemPrompt` — Returns the system prompt that instructs the orchestrator LLM
- `truncateForLog` — Truncates long strings for console logging
- `runWithCleanup` — Wraps a function with optional signal handling and session cleanup

### Tmux Isolation
All tmux commands go through `runTmux()` / `tmuxArgs()` which prepend `-L <socket>` to isolate from the user's real tmux. The socket name defaults to `gt-claude-loop`.

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
- Single-file architecture — all application code goes in `main.go`
- No external dependencies — stdlib only
- Error messages include the function name as prefix (e.g., `"createSession: new-session: ..."`)
- Input validation uses allowlists (e.g., `validateSessionName` permits only `[a-zA-Z0-9_-]`)
- Package-level vars (`pollInterval`, `stableWindow`, `startupSettleWindow`, `tmuxSocket`, `openRouterEndpoint`, `maxIterations`) are overridden in tests via helpers like `overrideTimers` and `setupIntegration`
- The autonomous loop completion signal is the literal string `TASK_COMPLETE` checked via `strings.Contains`