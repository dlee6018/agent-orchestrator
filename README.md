# go-orchestrator

A Go CLI tool that orchestrates Claude Code sessions via tmux. It manages a persistent tmux session running Claude CLI, provides an interactive chat loop, and automatically recovers from session or server crashes.

## How it works

1. Launches Claude CLI inside a dedicated tmux session (isolated via a custom socket)
2. Sends user input as keystrokes to the tmux pane
3. Polls the pane output until it stabilizes, indicating Claude has finished responding
4. Prints the full pane contents back to the terminal
5. If the tmux session or server dies, automatically restarts and retries

## Prerequisites

- Go 1.23+
- [tmux](https://github.com/tmux/tmux) installed and on PATH
- [Claude CLI](https://docs.anthropic.com/en/docs/claude-code) installed and on PATH

## Usage

```bash
go build -o go-orchestrator .
./go-orchestrator
```

This starts an interactive prompt. Type a message and press Enter to send it to Claude. Type `/quit` to exit.

## Environment variables

| Variable | Default | Description |
|---|---|---|
| `CLAUDE_TMUX_SESSION` | `gt-claude-loop` | tmux session name |
| `CLAUDE_TMUX_SOCKET` | `gt-claude-loop` | tmux socket name (isolates from user's tmux) |
| `CLAUDE_CMD` | `claude --dangerously-skip-permissions --setting-sources user` | Command to run inside the tmux session |
| `TERMINATE_WHEN_QUIT` | `false` | Kill the tmux session on `/quit` or signal (SIGINT/SIGTERM) |

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

- **`main` / `chatLoop`** -- Entry point and interactive read-eval-print loop
- **`ensureClaudeSession`** -- Creates or validates the tmux session, restarts if the pane is dead
- **`sendAndCaptureWithRecovery`** -- Sends a message, waits for output, retries once on recoverable failures
- **`waitForPaneUpdate`** -- Polls `capture-pane` until output changes and stabilizes for a configurable window
- **`waitForRuntimeReady`** -- Blocks until the tmux session is alive and the startup command hasn't crashed
- **`restartClaudeSession`** -- Kills and recreates the session from scratch
