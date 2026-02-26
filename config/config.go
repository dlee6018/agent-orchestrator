package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileName is the name of the persistent config file.
const FileName = "config.json"

// Config holds user-selected settings from the /setup wizard.
// All fields are strings because they map directly to env var values
// and are consumed by helpers.EnvBool/helpers.EnvOrDefault.
type Config struct {
	DefaultModel     string `json:"default_model"`
	OpenRouterModel  string `json:"openrouter_model"`
	DashboardEnabled string `json:"dashboard_enabled"`
	MultiAgentMode   string `json:"multi_agent_mode"`
	AutonomousMode   string `json:"autonomous_mode"`
	WorktreeMode     string `json:"worktree_mode"`
	SandboxMode      string `json:"sandbox_mode"`
}

// question describes a single wizard prompt with its numbered choices.
type question struct {
	label   string
	envVar  string
	choices []string
}

// wizardQuestions defines the ordered list of setup wizard prompts.
var wizardQuestions = []question{
	{"Select inner coding agent model", "DEFAULT_MODEL", []string{"gpt-5.3-codex", "claude-opus-4.6"}},
	{"Select orchestrator LLM model", "OPENROUTER_MODEL", []string{"anthropic/claude-opus-4.6"}},
	{"Enable web dashboard", "DASHBOARD_ENABLED", []string{"true", "false"}},
	{"Enable multi-agent mode", "MULTI_AGENT_MODE", []string{"true", "false"}},
	{"Enable autonomous mode", "AUTONOMOUS_MODE", []string{"true", "false"}},
	{"Enable worktree mode (Claude Code only)", "WORKTREE_MODE", []string{"true", "false"}},
	{"Enable sandbox mode (Claude Code only)", "SANDBOX_MODE", []string{"true", "false"}},
}

// LoadConfig reads config.json from dir and returns the parsed Config.
// Returns nil, nil if the file does not exist.
func LoadConfig(dir string) (*Config, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("LoadConfig: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("LoadConfig: unmarshal: %w", err)
	}
	return &cfg, nil
}

// ConfigDir returns the global config directory for go-orchestrator (~/.config/go-orchestrator).
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ConfigDir: %w", err)
	}
	return filepath.Join(home, ".config", "go-orchestrator"), nil
}

// SaveConfig writes the Config as pretty-printed JSON to config.json in dir.
// Creates the directory if it doesn't exist.
func SaveConfig(dir string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("SaveConfig: config is nil")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("SaveConfig: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("SaveConfig: marshal: %w", err)
	}
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("SaveConfig: write: %w", err)
	}
	return nil
}

// ApplyConfig sets environment variables for each non-empty field in the Config.
// Fields that are empty strings are skipped.
func ApplyConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	pairs := []struct {
		key, val string
	}{
		{"DEFAULT_MODEL", cfg.DefaultModel},
		{"OPENROUTER_MODEL", cfg.OpenRouterModel},
		{"DASHBOARD_ENABLED", cfg.DashboardEnabled},
		{"MULTI_AGENT_MODE", cfg.MultiAgentMode},
		{"AUTONOMOUS_MODE", cfg.AutonomousMode},
		{"WORKTREE_MODE", cfg.WorktreeMode},
		{"SANDBOX_MODE", cfg.SandboxMode},
	}
	for _, p := range pairs {
		if p.val != "" {
			os.Setenv(p.key, p.val)
		}
	}
}

// RunSetupWizard runs an interactive numbered-choice wizard, reading from the
// provided scanner and writing prompts to w. Returns the completed Config or
// an error if input ends prematurely.
func RunSetupWizard(scanner *bufio.Scanner, w io.Writer) (*Config, error) {
	fmt.Fprintln(w, "\n=== go-orchestrator Setup ===")
	fmt.Fprintln(w)

	answers := make([]string, len(wizardQuestions))
	for i, q := range wizardQuestions {
		answer, err := askQuestion(scanner, w, i+1, len(wizardQuestions), q)
		if err != nil {
			return nil, err
		}
		answers[i] = answer
	}

	cfg := &Config{
		DefaultModel:     answers[0],
		OpenRouterModel:  answers[1],
		DashboardEnabled: answers[2],
		MultiAgentMode:   answers[3],
		AutonomousMode:   answers[4],
		WorktreeMode:     answers[5],
		SandboxMode:      answers[6],
	}
	return cfg, nil
}

// askQuestion prints a single numbered-choice prompt and reads the user's selection.
// Invalid input re-prompts until a valid choice is made. Returns an error on EOF.
func askQuestion(scanner *bufio.Scanner, w io.Writer, num, total int, q question) (string, error) {
	for {
		fmt.Fprintf(w, "[%d/%d] %s (%s):\n", num, total, q.label, q.envVar)
		for j, c := range q.choices {
			fmt.Fprintf(w, "  %d) %s\n", j+1, c)
		}
		fmt.Fprintf(w, "Enter choice [1-%d]: ", len(q.choices))

		if !scanner.Scan() {
			return "", fmt.Errorf("RunSetupWizard: unexpected end of input at question %d", num)
		}
		input := strings.TrimSpace(scanner.Text())
		n, err := strconv.Atoi(input)
		if err != nil || n < 1 || n > len(q.choices) {
			fmt.Fprintf(w, "Invalid choice %q, please enter a number between 1 and %d.\n\n", input, len(q.choices))
			continue
		}
		fmt.Fprintln(w)
		return q.choices[n-1], nil
	}
}
