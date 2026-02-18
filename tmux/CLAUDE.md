# tmux

## Purpose

All tmux session management and I/O — session lifecycle, message sending, pane polling, text cleaning. No cross-package dependencies (stdlib only).

## Exported API

### Variables (test-overridable)

| Variable | Default | Description |
|---|---|---|
| `Socket` | `""` | tmux socket name for session isolation |
| `PollInterval` | 500ms | How often to poll for pane updates |
| `StableWindow` | 2s | How long pane content must be stable |
| `StartupSettleWindow` | 1.5s | Settle time after startup |
| `KeystrokeSleep` | 500ms | Pause between text and Enter |
| `MaxSendRetries` | 2 | 1 initial attempt + 1 retry |

### Functions

| Function | Description |
|---|---|
| `EnsureClaudeSession` | Creates or validates/restarts the tmux session |
| `SendAndCaptureWithRecovery` | Send + capture with one retry on recoverable failures |
| `SendMessage` | Sends text as literal keystrokes followed by Enter |
| `WaitForPaneUpdate` | Polls pane until content changes and stabilizes |
| `WaitForPaneUpdateWithCapture` | Testable core with injectable capture/checkAlive |
| `WaitForRuntimeReady` | Blocks until session is alive and startup didn't crash |
| `CapturePane` | Returns full visible text of the tmux pane |
| `CleanupSession` | Kills the tmux session, ignoring errors |
| `CleanPaneOutput` | Strips ANSI escapes, collapses blank lines, trims whitespace |
| `TruncateForLog` | Truncates long strings with "..." suffix |
| `ResolveStartupCommand` | Validates command, resolves binary via LookPath |
| `RunTmux` | Executes a tmux command with the configured socket |
| `TmuxArgs` | Prepends `-L <socket>` flag |
| `ShouldRecoverSession` | Checks if error warrants session restart |
| `ParsePaneStateLine` | Parses `dead\tstatus\tcommand` from list-panes |
| `IsShellEnvAssignment` | Detects KEY=VALUE tokens |

## Key Implementation Details

- All tmux commands go through `RunTmux()` / `TmuxArgs()` which prepend `-L <Socket>` to isolate from the user's real tmux
- `WaitForPaneUpdateWithCapture` accepts injectable capture and liveness-check functions for testability
- `SendAndCaptureWithRecovery` auto-restarts the session on recoverable failures

## Testing

```bash
go test -v ./tmux/                        # unit tests
go test -v -tags=integration ./...        # integration tests (at root)
```

Unit tests use `OverrideTimers(t)` to speed up polling. Integration tests live in the root `integration_test.go`.
