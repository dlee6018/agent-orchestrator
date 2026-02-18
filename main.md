# High-Level Call Diagram

```
main()
│
├── loadEnvFile(".env")            ← load .env file (won't override real env)
├── envOrDefault()                 ← resolve session name, socket, command
├── validateSessionName()          ← validate session + socket names
├── resolveStartupCommand()        ← resolve binary to abs path via LookPath
│   └── isShellEnvAssignment()
├── filepath.Abs() / os.Getwd()   ← resolve working directory (from arg or cwd)
│
├── ensureClaudeSession()          ← create or validate tmux session
│   ├── tmuxHasSession()
│   │   └── runTmux("has-session")
│   │       └── tmuxArgs()         ← prepend -L <socket>
│   │
│   ├── [if no session] createSession()
│   │   ├── runTmux("new-session", ...)
│   │   ├── setTmuxServerOptions()
│   │   │   └── runTmux("set-option", "exit-empty", "off")
│   │   └── configureSession()
│   │       └── runTmux("set-window-option", "remain-on-exit", "on")
│   │
│   ├── [if session exists] tmuxPaneState()
│   │   └── parsePaneStateLine()
│   │   └── [if dead] restartClaudeSession()
│   │       ├── runTmux("kill-session")
│   │       ├── createSession()           ← (same as above)
│   │       └── waitForRuntimeReady()     ← (same as below)
│   │
│   └── waitForRuntimeReady()      ← poll until session is alive + stable
│       ├── tmuxHasSession()
│       ├── tmuxPaneState()
│       ├── capturePane()
│       └── requireSessionAliveFor()    ← verify no fast crash
│           └── tmuxHasSession()  (loop)
│
├── envBool("TERMINATE_WHEN_QUIT")
│
├── runWithCleanup()               ← optional signal handler + session cleanup
│   ├── [if terminate] signal.Notify() → goroutine watches SIGINT/SIGTERM
│   │                                     └── cleanupSession()
│   └── fn()                       ← runs chatLoop or autonomousLoop
│
├── [AUTONOMOUS_MODE=false] chatLoop()       ← interactive mode
│   │
│   └── [per message] sendAndCaptureWithRecovery()    ← up to 2 attempts
│       │
│       ├── ensureClaudeSession()         ← re-validate before each msg
│       │
│       ├── sendMessage()
│       │   ├── runTmux("send-keys", "-l", message)   ← literal text
│       │   └── runTmux("send-keys", "C-m")           ← press Enter
│       │
│       ├── waitForPaneUpdate()
│       │   └── waitForPaneUpdateWithCapture()         ← poll loop
│       │       └── capturePane()  (repeated until stable)
│       │
│       └── [on recoverable error] shouldRecoverSession()
│           └── restartClaudeSession()    ← kill + recreate + wait ready
│
└── [AUTONOMOUS_MODE=true] autonomousLoop()  ← agent mode (default)
    │
    ├── buildSystemPrompt()                  ← LLM system instructions
    │
    └── [loop until TASK_COMPLETE or maxIterations]
        │
        ├── callOpenRouter()                 ← send conversation to LLM
        │   ├── json.Marshal(openRouterRequest)
        │   ├── http.NewRequest("POST", endpoint, ...)
        │   └── json.Unmarshal(openRouterResponse)
        │
        ├── [if reply contains "TASK_COMPLETE"] → return
        │
        ├── sendAndCaptureWithRecovery()     ← send LLM reply to Claude Code
        │   └── (same as chat mode above)
        │
        ├── [if "still working"] waitForPaneUpdate()  ← keep polling
        │
        ├── cleanPaneOutput()                ← strip ANSI, collapse blanks
        │
        └── append to messages[]             ← feed output back to LLM
```

## Summary of the Flow

1. **Startup** — Config is loaded from `.env` and env vars, names validated, the CLI binary resolved to an absolute path. A working directory is resolved from the first CLI argument or cwd.
2. **Session bootstrap** — `ensureClaudeSession` either creates a new tmux session or checks if an existing one is healthy (restarting it if the pane is dead). Then it polls with `waitForRuntimeReady` to confirm the process didn't crash on startup.
3. **Signal handler** — If `TERMINATE_WHEN_QUIT=true`, `runWithCleanup` registers a goroutine that listens for SIGINT/SIGTERM to clean up the tmux session on exit.
4. **Chat mode** (`AUTONOMOUS_MODE=false`) — Reads stdin line by line. Each message goes through `sendAndCaptureWithRecovery`, which ensures the session is alive, sends keystrokes via `send-keys`, then polls `capture-pane` until the output changes and stabilizes for `stableWindow` (2s). If tmux dies mid-message, it retries once after a full session restart.
5. **Autonomous mode** (`AUTONOMOUS_MODE=true`, default) — Prompts the user for a task description, then enters `autonomousLoop`. Each iteration calls the orchestrator LLM via OpenRouter, sends its reply as keystrokes to Claude Code, captures the pane output, cleans it, and feeds it back to the LLM as the next user message. The loop terminates when the LLM responds with `TASK_COMPLETE` or the iteration limit is reached. Consecutive API errors (3+) also abort the loop.
