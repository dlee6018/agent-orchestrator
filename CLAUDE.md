# go-orchestrator

## What This Is

A Go CLI that orchestrates Claude Code sessions via tmux. It runs Claude CLI inside a persistent tmux session, sends user input as keystrokes, polls the pane output until it stabilizes, and prints results back. It auto-recovers from tmux session and server crashes.

## Project Structure

- `main.go` — Entire application in a single file (entry point, tmux management, chat loop, recovery logic)
- `main_test.go` — Unit tests (no tmux required)
- `main_integration_test.go` — Integration tests (requires tmux, uses `//go:build integration` tag)
- `go.mod` — Go 1.23, module name `main`, no external dependencies
- `.env` — Runtime env vars (contains `TERMINATE_WHEN_QUIT=true`)

## Key Architecture

All code lives in `main.go` in a single `package main`. There are no subdirectories or packages.

### Core Flow
1. `main()` → resolves config from env vars → `ensureClaudeSession()` → `chatLoop()`
2. `chatLoop()` reads stdin, calls `sendAndCaptureWithRecovery()` per message
3. `sendAndCaptureWithRecovery()` ensures session → sends keystrokes → polls for stable output → retries once on recoverable failures
4. `waitForPaneUpdate()` polls `capture-pane` until output changes and stabilizes (configurable `stableWindow`)

### Key Functions
- `ensureClaudeSession` — Creates or validates the tmux session, restarts if dead
- `sendAndCaptureWithRecovery` — Send + capture with one retry on recoverable errors
- `waitForPaneUpdate` / `waitForPaneUpdateWithCapture` — Poll-based output detection
- `waitForRuntimeReady` — Blocks until tmux session is alive and startup command hasn't crashed
- `restartClaudeSession` — Kill + recreate session from scratch
- `resolveStartupCommand` — Validates command, resolves binary to absolute path via `LookPath`
- `shouldRecoverSession` — Checks error strings to decide if session restart is warranted

### Tmux Isolation
All tmux commands go through `runTmux()` / `tmuxArgs()` which prepend `-L <socket>` to isolate from the user's real tmux. The socket name defaults to `gt-claude-loop`.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `CLAUDE_TMUX_SESSION` | `gt-claude-loop` | tmux session name |
| `CLAUDE_TMUX_SOCKET` | `gt-claude-loop` | tmux socket name |
| `CLAUDE_CMD` | `claude --dangerously-skip-permissions --setting-sources user` | Command to run inside tmux |
| `TERMINATE_WHEN_QUIT` | `false` | Kill tmux session on `/quit` or signal |

## Build & Run

```bash
go build -o go-orchestrator .
./go-orchestrator
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

- Single-file architecture — all application code goes in `main.go`
- No external dependencies — stdlib only
- Error messages include the function name as prefix (e.g., `"createSession: new-session: ..."`)
- Input validation uses allowlists (e.g., `validateSessionName` permits only `[a-zA-Z0-9_-]`)
- Package-level vars (`pollInterval`, `stableWindow`, `startupSettleWindow`, `tmuxSocket`) are overridden in tests via helpers like `overrideTimers` and `setupIntegration`