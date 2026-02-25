# dashboard

## Purpose

SSE event broker and embedded web dashboard for real-time monitoring of the autonomous loop. No cross-package dependencies (stdlib only).

## Exported API

### Types

| Type | Description |
|---|---|
| `IterationEvent` | SSE event payload with type, iteration, timestamps, tokens, output, errors, agents |
| `TokenUsage` | Prompt/completion/total token counts |
| `AgentHealthStatus` | Per-agent health: role, session, alive, exit status |
| `RestartFunc` | Callback type `func(session string) error` for restarting dead sessions |
| `SSEBroker` | Fan-out broadcaster for SSE events to multiple clients |

### Functions

| Function | Description |
|---|---|
| `NewSSEBroker()` | Creates a new SSEBroker instance |
| `(*SSEBroker).Subscribe()` | Adds a client, returns event channel and unsubscribe function |
| `(*SSEBroker).Publish(event)` | Sends event to all clients (nil-safe, non-blocking) |
| `StartDashboard(broker, port, restartFn)` | Starts the HTTP server with optional restart callback, returns listening address |
| `OpenBrowser(url)` | Opens URL in default browser |

## Key Implementation Details

- `Publish` is nil-safe — calling it on a nil broker is a no-op
- `task_info` and `health_status` events are retained and replayed to late-joining subscribers
- Channel buffer is 64; slow clients have messages dropped (non-blocking send)
- Web assets are embedded via `//go:embed web/*` and served at `/`
- SSE endpoint at `/events` sends `data: {...}\n\n` formatted events

## Event Types

| Type | When |
|---|---|
| `task_info` | Initial task metadata (replayed to late joiners) |
| `iteration_start` | Iteration begins |
| `iteration_end` | Iteration completes (includes duration + tokens) |
| `error` | Iteration error (e.g. API failure) |
| `complete` | Task finished or aborted |
| `health_status` | Per-agent health update (replayed to late joiners) |

## Testing

```bash
go test -v ./dashboard/
```
