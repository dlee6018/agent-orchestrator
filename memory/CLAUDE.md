# memory

## Purpose

Persistent memory — load/save facts across sessions, extract MEMORY_SAVE lines from LLM replies, deduplicate and compact facts. No cross-package dependencies (uses `CompactFunc` callback to avoid importing orchestrator).

## Exported API

### Constants and Variables

| Symbol | Description |
|---|---|
| `FileName` | `"memory.json"` — the persistent memory file name |
| `MaxFacts` | Compaction threshold (default 50, overridable via `MEMORY_MAX_FACTS`) |

### Types

| Type | Description |
|---|---|
| `CompactFunc` | `func(prompt string) (string, error)` — callback for LLM-based compaction |

### Functions

| Function | Description |
|---|---|
| `LoadMemory(workDir)` | Reads memory.json, returns facts (nil if file missing) |
| `SaveMemory(workDir, facts)` | Writes facts to memory.json |
| `ExtractMemorySaves(reply)` | Extracts `MEMORY_SAVE:` lines, returns facts + cleaned reply |
| `DeduplicateMemory(facts)` | Removes duplicates preserving order |
| `CompactMemory(fn, facts)` | Uses callback to consolidate facts via LLM |

## Key Implementation Details

- `CompactMemory` accepts a `CompactFunc` callback instead of calling the OpenRouter API directly — this breaks the circular dependency between memory and orchestrator
- The orchestrator creates the callback: `func(prompt) { CallOpenRouter(apiKey, model, ...) }`
- `ExtractMemorySaves` strips MEMORY_SAVE lines from the reply before it's sent to Claude Code
- `SaveMemory(nil)` writes an empty JSON array

## Testing

```bash
go test -v ./memory/
```

Tests use simple callback functions instead of mock HTTP servers (since the API dependency is injected).
