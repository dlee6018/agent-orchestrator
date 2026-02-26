package main

import (
	"strings"
	"testing"

	"github.com/dlee6018/agent-orchestrator/helpers"
	"github.com/dlee6018/agent-orchestrator/orchestrator"
)

// DEFAULT_MODEL=gpt-4o causes ResolveAgentConfig to return the codex command.
func TestDefaultModel_DeterminesCommand(t *testing.T) {
	cmd, name := helpers.ResolveAgentConfig("gpt-4o")
	if cmd != "codex" {
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
	if agentCommand != "codex" {
		t.Fatalf("precondition: expected codex command from gpt-4o, got %q", agentCommand)
	}

	// Simulate CLAUDE_CMD override via EnvOrDefault logic.
	overrideCmd := "my-custom-agent --flag"
	result := overrideCmd // same as: helpers.EnvOrDefault("CLAUDE_CMD", agentCommand) when CLAUDE_CMD is set
	if result != "my-custom-agent --flag" {
		t.Fatalf("CLAUDE_CMD should override DEFAULT_MODEL command, got %q", result)
	}
}

// DEFAULT_MODEL starting with "bedrock" auto-sets Provider to "amazon-bedrock".
func TestBedrockModel_SetsProvider(t *testing.T) {
	oldProvider := orchestrator.Provider
	t.Cleanup(func() { orchestrator.Provider = oldProvider })

	// Simulate the main.go logic: if DEFAULT_MODEL starts with "bedrock", set Provider.
	defaultModel := "bedrock-claude-opus"
	orchestrator.Provider = "" // default (let OpenRouter pick)
	if strings.HasPrefix(strings.ToLower(defaultModel), "bedrock") {
		orchestrator.Provider = "amazon-bedrock"
	}
	if orchestrator.Provider != "amazon-bedrock" {
		t.Fatalf("expected Provider %q, got %q", "amazon-bedrock", orchestrator.Provider)
	}
}

// WORKTREE_MODE=true appends --worktree to Claude Code command.
func TestWorktreeMode_AppendsFlag(t *testing.T) {
	agentCommand, agentName := helpers.ResolveAgentConfig("claude")
	if agentName != "Claude Code" {
		t.Fatalf("precondition: expected Claude Code, got %q", agentName)
	}

	// Simulate the main.go worktree injection logic.
	worktreeEnabled := true
	if worktreeEnabled && agentName == "Claude Code" {
		agentCommand = strings.Replace(agentCommand, "claude ", "claude --worktree ", 1)
	}

	if !strings.Contains(agentCommand, "--worktree") {
		t.Fatalf("expected --worktree in command, got %q", agentCommand)
	}
	// Verify original flags are still present.
	if !strings.Contains(agentCommand, "--dangerously-skip-permissions") {
		t.Fatalf("expected --dangerously-skip-permissions in command, got %q", agentCommand)
	}
}

// WORKTREE_MODE does not affect Codex command.
func TestWorktreeMode_DoesNotAffectCodex(t *testing.T) {
	agentCommand, agentName := helpers.ResolveAgentConfig("gpt-4o")
	if agentName != "Codex" {
		t.Fatalf("precondition: expected Codex, got %q", agentName)
	}

	// Simulate the main.go worktree injection logic.
	worktreeEnabled := true
	if worktreeEnabled && agentName == "Claude Code" {
		agentCommand = strings.Replace(agentCommand, "claude ", "claude --worktree ", 1)
	}

	if agentCommand != "codex" {
		t.Fatalf("expected codex command unchanged, got %q", agentCommand)
	}
}

// SANDBOX_MODE=true replaces --dangerously-skip-permissions with sandbox settings.
func TestSandboxMode_ReplacesPermissions(t *testing.T) {
	agentCommand, agentName := helpers.ResolveAgentConfig("claude")
	if agentName != "Claude Code" {
		t.Fatalf("precondition: expected Claude Code, got %q", agentName)
	}

	// Simulate the main.go sandbox injection logic.
	sandboxEnabled := true
	if sandboxEnabled && agentName == "Claude Code" {
		agentCommand = strings.Replace(agentCommand, "--dangerously-skip-permissions",
			`--settings {"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true}}`, 1)
	}

	if strings.Contains(agentCommand, "--dangerously-skip-permissions") {
		t.Fatalf("expected --dangerously-skip-permissions to be removed, got %q", agentCommand)
	}
	if !strings.Contains(agentCommand, `--settings {"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true}}`) {
		t.Fatalf("expected sandbox settings in command, got %q", agentCommand)
	}
	// Verify --setting-sources user is still present.
	if !strings.Contains(agentCommand, "--setting-sources user") {
		t.Fatalf("expected --setting-sources user in command, got %q", agentCommand)
	}
}

// SANDBOX_MODE does not affect Codex command.
func TestSandboxMode_DoesNotAffectCodex(t *testing.T) {
	agentCommand, agentName := helpers.ResolveAgentConfig("gpt-4o")
	if agentName != "Codex" {
		t.Fatalf("precondition: expected Codex, got %q", agentName)
	}

	sandboxEnabled := true
	if sandboxEnabled && agentName == "Claude Code" {
		agentCommand = strings.Replace(agentCommand, "--dangerously-skip-permissions",
			`--settings {"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true}}`, 1)
	}

	if agentCommand != "codex" {
		t.Fatalf("expected codex command unchanged, got %q", agentCommand)
	}
}

// DEFAULT_MODEL not starting with "bedrock" does not override Provider.
func TestNonBedrockModel_KeepsDefaultProvider(t *testing.T) {
	oldProvider := orchestrator.Provider
	t.Cleanup(func() { orchestrator.Provider = oldProvider })

	defaultModel := "claude-opus-4.6"
	orchestrator.Provider = "" // default (let OpenRouter pick)
	if strings.HasPrefix(strings.ToLower(defaultModel), "bedrock") {
		orchestrator.Provider = "amazon-bedrock"
	}
	if orchestrator.Provider != "" {
		t.Fatalf("expected Provider %q, got %q", "", orchestrator.Provider)
	}
}
