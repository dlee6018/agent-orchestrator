# orchestrator

## Purpose

Autonomous agent loop and OpenRouter API client. Drives a coding agent (Claude Code or Codex) via an LLM that decides what to type, captures output, and feeds it back until TASK_COMPLETE. Depends on `tmux`, `memory`, and `dashboard` packages.

## Exported API

### Constants and Variables

| Symbol | Description |
|---|---|
| `Endpoint` | OpenRouter API URL (var, overridable in tests) |
| `MaxIterations` | Safety cap on loop iterations (0 = unlimited, var) |
| `DefaultModel` | `"anthropic/claude-opus-4.6"` |
| `TaskCompleteMarker` | `"TASK_COMPLETE"` |

### Types

| Type | Description |
|---|---|
| `Message` | Chat message (`role` + `content`) |
| `Request` | OpenRouter request body |
| `Choice` | Single completion choice |
| `Usage` | Token counts (prompt/completion/total) |
| `Response` | OpenRouter response body |
| `ErrorResponse` | OpenRouter error response |

### Functions

| Function | Description |
|---|---|
| `CallOpenRouter(apiKey, model, messages, temp)` | Sends chat completion request, returns reply + usage |
| `BuildSystemPrompt(agentName, memories)` | Returns system prompt parameterized with the agent display name (with optional memory section) |
| `AutonomousLoop(session, workDir, command, apiKey, model, task, agentName, broker, memories)` | Main agent loop; `agentName` is the display name (e.g. "Claude Code", "Codex") used in prompts and logs |

## Key Implementation Details

- `AutonomousLoop` calls `tmux.SendAndCaptureWithRecovery` to interact with the coding agent
- `agentName` is threaded through prompts and log output so messages reference the correct agent (e.g. "ORCHESTRATOR → CLAUDE CODE" or "ORCHESTRATOR → CODEX")
- Memory saves are extracted via `memory.ExtractMemorySaves` and stripped before forwarding
- Compaction uses a callback: `memory.CompactMemory(fn, facts)` where fn wraps `CallOpenRouter`
- SSE events are published to `dashboard.SSEBroker` throughout the loop
- 3 consecutive API errors abort the loop; transient errors retry with 5s backoff
- Files split: `types.go` has API types + `CallOpenRouter` + `BuildSystemPrompt`; `orchestrator.go` has `AutonomousLoop`

## Testing

```bash
go test -v ./orchestrator/                # unit tests
go test -v -tags=integration ./...        # integration tests (at root)
```

Unit tests use mock HTTP servers for `CallOpenRouter`. Integration tests with real tmux are in the root `integration_test.go`.
