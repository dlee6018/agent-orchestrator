package helpers

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadEnvFile returns nil for a missing file.
func TestLoadEnvFile_MissingFile(t *testing.T) {
	err := LoadEnvFile(filepath.Join(t.TempDir(), ".env"))
	if err != nil {
		t.Fatalf("missing .env should not error: %v", err)
	}
}

// LoadEnvFile parses KEY=VALUE lines and sets env vars.
func TestLoadEnvFile_SetsVars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "TESTENV_A=hello\nTESTENV_B=world\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	os.Unsetenv("TESTENV_A")
	os.Unsetenv("TESTENV_B")
	t.Cleanup(func() {
		os.Unsetenv("TESTENV_A")
		os.Unsetenv("TESTENV_B")
	})

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("TESTENV_A"); got != "hello" {
		t.Fatalf("TESTENV_A: got %q, want %q", got, "hello")
	}
	if got := os.Getenv("TESTENV_B"); got != "world" {
		t.Fatalf("TESTENV_B: got %q, want %q", got, "world")
	}
}

// LoadEnvFile does not overwrite existing env vars.
func TestLoadEnvFile_NoOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("TESTENV_C=from_file\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	os.Setenv("TESTENV_C", "already_set")
	t.Cleanup(func() { os.Unsetenv("TESTENV_C") })

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("TESTENV_C"); got != "already_set" {
		t.Fatalf("TESTENV_C: got %q, want %q (should not overwrite)", got, "already_set")
	}
}

// LoadEnvFile strips surrounding quotes from values.
func TestLoadEnvFile_StripsQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "TESTENV_D=\"double quoted\"\nTESTENV_E='single quoted'\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	os.Unsetenv("TESTENV_D")
	os.Unsetenv("TESTENV_E")
	t.Cleanup(func() {
		os.Unsetenv("TESTENV_D")
		os.Unsetenv("TESTENV_E")
	})

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("TESTENV_D"); got != "double quoted" {
		t.Fatalf("TESTENV_D: got %q, want %q", got, "double quoted")
	}
	if got := os.Getenv("TESTENV_E"); got != "single quoted" {
		t.Fatalf("TESTENV_E: got %q, want %q", got, "single quoted")
	}
}

// LoadEnvFile ignores comments and blank lines.
func TestLoadEnvFile_IgnoresCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := "# this is a comment\n\nTESTENV_F=value\n\n# another comment\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	os.Unsetenv("TESTENV_F")
	t.Cleanup(func() { os.Unsetenv("TESTENV_F") })

	if err := LoadEnvFile(path); err != nil {
		t.Fatalf("LoadEnvFile: %v", err)
	}
	if got := os.Getenv("TESTENV_F"); got != "value" {
		t.Fatalf("TESTENV_F: got %q, want %q", got, "value")
	}
}

// EnvOrDefault returns the env var when set, fallback when empty/unset.
func TestEnvOrDefault(t *testing.T) {
	os.Setenv("TESTENV_G", "present")
	t.Cleanup(func() { os.Unsetenv("TESTENV_G") })

	if got := EnvOrDefault("TESTENV_G", "fallback"); got != "present" {
		t.Fatalf("set var: got %q, want %q", got, "present")
	}
	if got := EnvOrDefault("TESTENV_MISSING", "fallback"); got != "fallback" {
		t.Fatalf("missing var: got %q, want %q", got, "fallback")
	}
}

// EnvOrDefault returns fallback for whitespace-only values.
func TestEnvOrDefault_WhitespaceOnly(t *testing.T) {
	os.Setenv("TESTENV_H", "   ")
	t.Cleanup(func() { os.Unsetenv("TESTENV_H") })

	if got := EnvOrDefault("TESTENV_H", "fallback"); got != "fallback" {
		t.Fatalf("whitespace var: got %q, want %q", got, "fallback")
	}
}

// EnvBool parses true/false variants and falls back for unset.
func TestEnvBool(t *testing.T) {
	tests := []struct {
		value    string
		fallback bool
		want     bool
	}{
		{"true", false, true},
		{"1", false, true},
		{"yes", false, true},
		{"TRUE", false, true},
		{"false", true, false},
		{"0", true, false},
		{"no", true, false},
		{"FALSE", true, false},
	}
	for _, tt := range tests {
		os.Setenv("TESTENV_BOOL", tt.value)
		got := EnvBool("TESTENV_BOOL", tt.fallback)
		if got != tt.want {
			t.Errorf("EnvBool(%q, %v) = %v, want %v", tt.value, tt.fallback, got, tt.want)
		}
	}
	os.Unsetenv("TESTENV_BOOL")
}

// EnvBool returns fallback when the variable is unset.
func TestEnvBool_Unset(t *testing.T) {
	os.Unsetenv("TESTENV_BOOL_MISSING")
	if got := EnvBool("TESTENV_BOOL_MISSING", true); got != true {
		t.Fatalf("unset with fallback=true: got %v", got)
	}
	if got := EnvBool("TESTENV_BOOL_MISSING", false); got != false {
		t.Fatalf("unset with fallback=false: got %v", got)
	}
}

// ValidateSessionName accepts valid names.
func TestValidateSessionName_Valid(t *testing.T) {
	valid := []string{"abc", "my-session", "test_123", "A-B-C", "a1b2c3"}
	for _, name := range valid {
		if err := ValidateSessionName(name); err != nil {
			t.Errorf("ValidateSessionName(%q) should be valid: %v", name, err)
		}
	}
}

// ValidateSessionName rejects empty and names with invalid characters.
func TestValidateSessionName_Invalid(t *testing.T) {
	invalid := []string{"", "has space", "has/slash", "has@symbol", "dot.name"}
	for _, name := range invalid {
		if err := ValidateSessionName(name); err == nil {
			t.Errorf("ValidateSessionName(%q) should be invalid", name)
		}
	}
}

// ResolveAgentConfig returns Claude Code command and display name for claude models.
func TestResolveAgentConfig_ClaudeModel(t *testing.T) {
	cmd, name := ResolveAgentConfig("claude-opus-4")
	if cmd != "claude --dangerously-skip-permissions --setting-sources user" {
		t.Fatalf("command: got %q, want claude command", cmd)
	}
	if name != "Claude Code" {
		t.Fatalf("name: got %q, want %q", name, "Claude Code")
	}
}

// ResolveAgentConfig returns codex command and display name for gpt models.
func TestResolveAgentConfig_GPTModel(t *testing.T) {
	cmd, name := ResolveAgentConfig("gpt-4o")
	if cmd != "codex --approval-mode full-auto" {
		t.Fatalf("command: got %q, want %q", cmd, "codex --approval-mode full-auto")
	}
	if name != "Codex" {
		t.Fatalf("name: got %q, want %q", name, "Codex")
	}
}

// ResolveAgentConfig defaults to Claude Code for empty string.
func TestResolveAgentConfig_Empty(t *testing.T) {
	cmd, name := ResolveAgentConfig("")
	if cmd != "claude --dangerously-skip-permissions --setting-sources user" {
		t.Fatalf("command: got %q, want claude command", cmd)
	}
	if name != "Claude Code" {
		t.Fatalf("name: got %q, want %q", name, "Claude Code")
	}
}

// ResolveAgentConfig defaults to Claude Code for unknown model prefix.
func TestResolveAgentConfig_UnknownPrefix(t *testing.T) {
	cmd, name := ResolveAgentConfig("llama-3.1-70b")
	if cmd != "claude --dangerously-skip-permissions --setting-sources user" {
		t.Fatalf("command: got %q, want claude command", cmd)
	}
	if name != "Claude Code" {
		t.Fatalf("name: got %q, want %q", name, "Claude Code")
	}
}

// ResolveAgentConfig handles case-insensitive model prefix matching.
func TestResolveAgentConfig_CaseInsensitive(t *testing.T) {
	cmd, name := ResolveAgentConfig("GPT-4o")
	if cmd != "codex --approval-mode full-auto" {
		t.Fatalf("command: got %q, want %q", cmd, "codex --approval-mode full-auto")
	}
	if name != "Codex" {
		t.Fatalf("name: got %q, want %q", name, "Codex")
	}
}
