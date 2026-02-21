package helpers

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile reads a .env file and sets any KEY=VALUE pairs as environment
// variables (only if not already set in the environment). Lines starting with
// '#' and blank lines are ignored.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no .env file is fine
		}
		return fmt.Errorf("LoadEnvFile: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		// key=value format in .env
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		// Strip matching surrounding quotes (single or double).
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		// Don't overwrite variables already set in the real environment.
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

// EnvOrDefault returns the env var value for key, or fallback if unset/empty.
func EnvOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

// EnvBool parses a boolean env var (true/1/yes or false/0/no), returning fallback if unset.
func EnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	case "":
		return fallback
	default:
		fmt.Fprintf(os.Stderr, "warning: unrecognized boolean value %q for %s, using default %v\n", v, key, fallback)
		return fallback
	}
}

// ResolveAgentConfig maps a DEFAULT_MODEL value to the CLI command and display name.
// Models starting with "gpt" (case-insensitive) resolve to Codex; all others default to Claude Code.
func ResolveAgentConfig(defaultModel string) (command, displayName string) {
	if strings.HasPrefix(strings.ToLower(defaultModel), "gpt") {
		return "codex --approval-mode full-auto", "Codex"
	}
	return "claude --dangerously-skip-permissions --setting-sources user", "Claude Code"
}

// ValidateSessionName rejects names with characters outside [a-zA-Z0-9_-].
func ValidateSessionName(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return fmt.Errorf("invalid character %q in name %q; only alphanumeric, hyphens, and underscores are allowed", r, name)
		}
	}
	return nil
}
