# helpers

## Purpose

Pure utility functions for environment variable handling and input validation. No cross-package dependencies.

## Exported API

| Function | Description |
|---|---|
| `LoadEnvFile(path)` | Parses `.env` file, sets KEY=VALUE pairs (won't overwrite existing vars) |
| `EnvOrDefault(key, fallback)` | Returns env var value, or fallback if unset/empty/whitespace |
| `EnvBool(key, fallback)` | Parses boolean env var (true/1/yes or false/0/no) |
| `ValidateSessionName(name)` | Rejects names with characters outside `[a-zA-Z0-9_-]` |

## Testing

```bash
go test -v ./helpers/
```

All tests are unit tests with no external dependencies.
