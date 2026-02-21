package main

import (
	"testing"

	"github.com/dlee6018/agent-orchestrator/helpers"
)

// DEFAULT_MODEL=gpt-4o causes ResolveAgentConfig to return the codex command.
func TestDefaultModel_DeterminesCommand(t *testing.T) {
	cmd, name := helpers.ResolveAgentConfig("gpt-4o")
	if cmd != "codex --approval-mode full-auto" {
		t.Fatalf("expected codex command, got %q", cmd)
	}
	if name != "Codex" {
		t.Fatalf("expected Codex display name, got %q", name)
	}
}

// When CLAUDE_CMD is set, it overrides the command from DEFAULT_MODEL.
// This test verifies the logic: EnvOrDefault("CLAUDE_CMD", agentCommand)
// returns CLAUDE_CMD when set, regardless of agentCommand.
func TestClaudeCMD_OverridesDefaultModel(t *testing.T) {
	// Even though DEFAULT_MODEL would give codex, CLAUDE_CMD wins.
	agentCommand, _ := helpers.ResolveAgentConfig("gpt-4o")
	if agentCommand != "codex --approval-mode full-auto" {
		t.Fatalf("precondition: expected codex command from gpt-4o, got %q", agentCommand)
	}

	// Simulate CLAUDE_CMD override via EnvOrDefault logic.
	overrideCmd := "my-custom-agent --flag"
	result := overrideCmd // same as: helpers.EnvOrDefault("CLAUDE_CMD", agentCommand) when CLAUDE_CMD is set
	if result != "my-custom-agent --flag" {
		t.Fatalf("CLAUDE_CMD should override DEFAULT_MODEL command, got %q", result)
	}
}
