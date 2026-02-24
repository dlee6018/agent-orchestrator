package config

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigDir_ReturnsPath verifies that ConfigDir returns a path rooted in the
// user's home directory and ending in .config/go-orchestrator.
func TestConfigDir_ReturnsPath(t *testing.T) {
	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	suffix := filepath.Join(".config", "go-orchestrator")
	if !strings.HasSuffix(dir, suffix) {
		t.Errorf("ConfigDir() = %q, want suffix %q", dir, suffix)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir failed: %v", err)
	}
	want := filepath.Join(home, ".config", "go-orchestrator")
	if dir != want {
		t.Errorf("ConfigDir() = %q, want %q", dir, want)
	}
}

// TestSaveConfig_CreatesDir verifies that SaveConfig creates the parent directory when it doesn't exist.
func TestSaveConfig_CreatesDir(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "sub", "dir")

	cfg := &Config{DefaultModel: "test-model"}
	if err := SaveConfig(nested, cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Verify file was created in the nested directory.
	path := filepath.Join(nested, FileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file not found at %s: %v", path, err)
	}

	// Verify round-trip.
	loaded, err := LoadConfig(nested)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if loaded.DefaultModel != "test-model" {
		t.Errorf("DefaultModel = %q, want %q", loaded.DefaultModel, "test-model")
	}
}

// TestLoadConfig_MissingFile verifies that a missing config.json returns nil, nil.
func TestLoadConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config, got %+v", cfg)
	}
}

// TestLoadConfig_ValidFile verifies that a well-formed config.json is parsed correctly.
func TestLoadConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	data := `{
  "default_model": "gpt-5.3-codex",
  "openrouter_model": "anthropic/claude-opus-4.6",
  "dashboard_enabled": "true",
  "multi_agent_mode": "false",
  "terminate_when_quit": "true",
  "autonomous_mode": "true"
}`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DefaultModel != "gpt-5.3-codex" {
		t.Errorf("DefaultModel = %q, want %q", cfg.DefaultModel, "gpt-5.3-codex")
	}
	if cfg.OpenRouterModel != "anthropic/claude-opus-4.6" {
		t.Errorf("OpenRouterModel = %q, want %q", cfg.OpenRouterModel, "anthropic/claude-opus-4.6")
	}
	if cfg.DashboardEnabled != "true" {
		t.Errorf("DashboardEnabled = %q, want %q", cfg.DashboardEnabled, "true")
	}
	if cfg.MultiAgentMode != "false" {
		t.Errorf("MultiAgentMode = %q, want %q", cfg.MultiAgentMode, "false")
	}
	if cfg.TerminateWhenQuit != "true" {
		t.Errorf("TerminateWhenQuit = %q, want %q", cfg.TerminateWhenQuit, "true")
	}
	if cfg.AutonomousMode != "true" {
		t.Errorf("AutonomousMode = %q, want %q", cfg.AutonomousMode, "true")
	}
}

// TestLoadConfig_InvalidJSON verifies that malformed JSON returns an error.
func TestLoadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{bad json}"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(dir)
	if err == nil {
		t.Fatalf("expected error for invalid JSON, got config %+v", cfg)
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("error should mention unmarshal, got %v", err)
	}
}

// TestSaveConfig_RoundTrip verifies that saving then loading returns the same struct.
func TestSaveConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &Config{
		DefaultModel:      "claude-opus-4.6",
		OpenRouterModel:   "anthropic/claude-opus-4.6",
		DashboardEnabled:  "false",
		MultiAgentMode:    "true",
		TerminateWhenQuit: "false",
		AutonomousMode:    "true",
	}

	if err := SaveConfig(dir, original); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	loaded, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if *loaded != *original {
		t.Errorf("round-trip mismatch:\n  got  %+v\n  want %+v", loaded, original)
	}
}

// TestSaveConfig_NilConfig verifies that saving a nil Config returns an error.
func TestSaveConfig_NilConfig(t *testing.T) {
	dir := t.TempDir()
	err := SaveConfig(dir, nil)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("error should mention nil, got %v", err)
	}
}

// TestApplyConfig_SetsEnvVars verifies that all 6 env vars are set correctly.
func TestApplyConfig_SetsEnvVars(t *testing.T) {
	keys := []string{
		"DEFAULT_MODEL", "OPENROUTER_MODEL", "DASHBOARD_ENABLED",
		"MULTI_AGENT_MODE", "TERMINATE_WHEN_QUIT", "AUTONOMOUS_MODE",
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}

	cfg := &Config{
		DefaultModel:      "gpt-5.3-codex",
		OpenRouterModel:   "anthropic/claude-opus-4.6",
		DashboardEnabled:  "true",
		MultiAgentMode:    "false",
		TerminateWhenQuit: "true",
		AutonomousMode:    "false",
	}
	ApplyConfig(cfg)

	expected := map[string]string{
		"DEFAULT_MODEL":       "gpt-5.3-codex",
		"OPENROUTER_MODEL":    "anthropic/claude-opus-4.6",
		"DASHBOARD_ENABLED":   "true",
		"MULTI_AGENT_MODE":    "false",
		"TERMINATE_WHEN_QUIT": "true",
		"AUTONOMOUS_MODE":     "false",
	}
	for k, want := range expected {
		got := os.Getenv(k)
		if got != want {
			t.Errorf("env %s = %q, want %q", k, got, want)
		}
	}
}

// TestApplyConfig_OverwritesExisting verifies that ApplyConfig overwrites pre-existing env vars.
func TestApplyConfig_OverwritesExisting(t *testing.T) {
	t.Setenv("DEFAULT_MODEL", "old-value")
	t.Setenv("AUTONOMOUS_MODE", "old-value")

	cfg := &Config{
		DefaultModel:   "new-model",
		AutonomousMode: "true",
	}
	ApplyConfig(cfg)

	if got := os.Getenv("DEFAULT_MODEL"); got != "new-model" {
		t.Errorf("DEFAULT_MODEL = %q, want %q", got, "new-model")
	}
	if got := os.Getenv("AUTONOMOUS_MODE"); got != "true" {
		t.Errorf("AUTONOMOUS_MODE = %q, want %q", got, "true")
	}
}

// TestApplyConfig_SkipsEmptyFields verifies that empty string fields are not set as env vars.
func TestApplyConfig_SkipsEmptyFields(t *testing.T) {
	t.Setenv("DEFAULT_MODEL", "should-stay")
	t.Setenv("AUTONOMOUS_MODE", "should-stay")

	cfg := &Config{
		DefaultModel:   "",
		AutonomousMode: "",
	}
	ApplyConfig(cfg)

	if got := os.Getenv("DEFAULT_MODEL"); got != "should-stay" {
		t.Errorf("DEFAULT_MODEL = %q, want %q (should not be overwritten)", got, "should-stay")
	}
	if got := os.Getenv("AUTONOMOUS_MODE"); got != "should-stay" {
		t.Errorf("AUTONOMOUS_MODE = %q, want %q (should not be overwritten)", got, "should-stay")
	}
}

// TestRunSetupWizard_HappyPath simulates valid input for all 6 questions.
func TestRunSetupWizard_HappyPath(t *testing.T) {
	// Answers: 1 (gpt-5.3-codex), 1 (anthropic/claude-opus-4.6), 2 (false), 1 (true), 2 (false), 1 (true)
	input := "1\n1\n2\n1\n2\n1\n"
	scanner := bufio.NewScanner(strings.NewReader(input))
	var buf bytes.Buffer

	cfg, err := RunSetupWizard(scanner, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.DefaultModel != "gpt-5.3-codex" {
		t.Errorf("DefaultModel = %q, want %q", cfg.DefaultModel, "gpt-5.3-codex")
	}
	if cfg.OpenRouterModel != "anthropic/claude-opus-4.6" {
		t.Errorf("OpenRouterModel = %q, want %q", cfg.OpenRouterModel, "anthropic/claude-opus-4.6")
	}
	if cfg.DashboardEnabled != "false" {
		t.Errorf("DashboardEnabled = %q, want %q", cfg.DashboardEnabled, "false")
	}
	if cfg.MultiAgentMode != "true" {
		t.Errorf("MultiAgentMode = %q, want %q", cfg.MultiAgentMode, "true")
	}
	if cfg.TerminateWhenQuit != "false" {
		t.Errorf("TerminateWhenQuit = %q, want %q", cfg.TerminateWhenQuit, "false")
	}
	if cfg.AutonomousMode != "true" {
		t.Errorf("AutonomousMode = %q, want %q", cfg.AutonomousMode, "true")
	}

	// Verify the output contains wizard header.
	output := buf.String()
	if !strings.Contains(output, "=== go-orchestrator Setup ===") {
		t.Error("output should contain wizard header")
	}
}

// TestRunSetupWizard_InvalidThenValid verifies that invalid input re-prompts and eventually succeeds.
func TestRunSetupWizard_InvalidThenValid(t *testing.T) {
	// First question: "abc" (invalid), "0" (out of range), "99" (out of range), then "2" (valid).
	// Remaining 5 questions answered with "1".
	input := "abc\n0\n99\n2\n1\n1\n1\n1\n1\n"
	scanner := bufio.NewScanner(strings.NewReader(input))
	var buf bytes.Buffer

	cfg, err := RunSetupWizard(scanner, &buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First question choice 2 = "claude-opus-4.6"
	if cfg.DefaultModel != "claude-opus-4.6" {
		t.Errorf("DefaultModel = %q, want %q", cfg.DefaultModel, "claude-opus-4.6")
	}

	// Verify error messages appeared in output.
	output := buf.String()
	if !strings.Contains(output, "Invalid choice") {
		t.Error("output should contain invalid choice error messages")
	}
}

// TestRunSetupWizard_EOF verifies that premature EOF returns an error.
func TestRunSetupWizard_EOF(t *testing.T) {
	// Only provide input for 2 questions, then EOF.
	input := "1\n1\n"
	scanner := bufio.NewScanner(strings.NewReader(input))
	var buf bytes.Buffer

	_, err := RunSetupWizard(scanner, &buf)
	if err == nil {
		t.Fatal("expected error on premature EOF")
	}
	if !strings.Contains(err.Error(), "unexpected end of input") {
		t.Errorf("error should mention unexpected end of input, got %v", err)
	}
}
