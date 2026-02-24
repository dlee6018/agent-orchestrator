package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// parseAgentDirective tests
// ---------------------------------------------------------------------------

// Valid JSON directive is parsed correctly.
func TestParseAgentDirective_ValidJSON(t *testing.T) {
	d, err := parseAgentDirective(`{"agent":"planner","message":"analyze code"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Agent != "planner" {
		t.Fatalf("agent: got %q, want %q", d.Agent, "planner")
	}
	if d.Message != "analyze code" {
		t.Fatalf("message: got %q, want %q", d.Message, "analyze code")
	}
}

// JSON embedded in surrounding text is extracted.
func TestParseAgentDirective_JSONWithSurroundingText(t *testing.T) {
	input := `Sure, I'll send this to the planner: {"agent":"executor","message":"run tests"} and that should work.`
	d, err := parseAgentDirective(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Agent != "executor" {
		t.Fatalf("agent: got %q, want %q", d.Agent, "executor")
	}
	if d.Message != "run tests" {
		t.Fatalf("message: got %q, want %q", d.Message, "run tests")
	}
}

// Missing agent field returns error.
func TestParseAgentDirective_MissingAgent(t *testing.T) {
	_, err := parseAgentDirective(`{"message":"hello"}`)
	if err == nil {
		t.Fatal("expected error for missing agent field")
	}
	if !strings.Contains(err.Error(), "missing required fields") {
		t.Fatalf("error should mention missing fields: %v", err)
	}
}

// Missing message field returns error.
func TestParseAgentDirective_MissingMessage(t *testing.T) {
	_, err := parseAgentDirective(`{"agent":"planner"}`)
	if err == nil {
		t.Fatal("expected error for missing message field")
	}
	if !strings.Contains(err.Error(), "missing required fields") {
		t.Fatalf("error should mention missing fields: %v", err)
	}
}

// Empty input returns error.
func TestParseAgentDirective_EmptyInput(t *testing.T) {
	_, err := parseAgentDirective("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty reply") {
		t.Fatalf("error should mention empty reply: %v", err)
	}
}

// No JSON in reply returns error.
func TestParseAgentDirective_NoJSON(t *testing.T) {
	_, err := parseAgentDirective("just some plain text without braces")
	if err == nil {
		t.Fatal("expected error when no JSON present")
	}
	if !strings.Contains(err.Error(), "no JSON object found") {
		t.Fatalf("error should mention no JSON found: %v", err)
	}
}

// ---------------------------------------------------------------------------
// findAgent tests
// ---------------------------------------------------------------------------

// Valid role name finds the agent.
func TestFindAgent_ValidRole(t *testing.T) {
	agents := []AgentState{
		{Role: AgentPlanner, Session: "s-planner"},
		{Role: AgentExecutor, Session: "s-executor"},
		{Role: AgentVerifier, Session: "s-verifier"},
	}
	ag, err := findAgent(agents, "executor")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ag.Role != AgentExecutor {
		t.Fatalf("role: got %q, want %q", ag.Role, AgentExecutor)
	}
	if ag.Session != "s-executor" {
		t.Fatalf("session: got %q, want %q", ag.Session, "s-executor")
	}
}

// Unknown role returns error.
func TestFindAgent_UnknownRole(t *testing.T) {
	agents := []AgentState{
		{Role: AgentPlanner, Session: "s-planner"},
	}
	_, err := findAgent(agents, "debugger")
	if err == nil {
		t.Fatal("expected error for unknown role")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("error should mention unknown agent: %v", err)
	}
}

// Empty name returns error.
func TestFindAgent_EmptyName(t *testing.T) {
	agents := []AgentState{
		{Role: AgentPlanner, Session: "s-planner"},
	}
	_, err := findAgent(agents, "")
	if err == nil {
		t.Fatal("expected error for empty name")
	}
	if !strings.Contains(err.Error(), "empty agent name") {
		t.Fatalf("error should mention empty name: %v", err)
	}
}

// Case-insensitive lookup works.
func TestFindAgent_CaseInsensitive(t *testing.T) {
	agents := []AgentState{
		{Role: AgentVerifier, Session: "s-verifier"},
	}
	ag, err := findAgent(agents, "VERIFIER")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ag.Role != AgentVerifier {
		t.Fatalf("role: got %q, want %q", ag.Role, AgentVerifier)
	}
}

// ---------------------------------------------------------------------------
// BuildMultiAgentSystemPrompt tests
// ---------------------------------------------------------------------------

// Multi-agent prompt contains all three agent role names.
func TestBuildMultiAgentSystemPrompt_ContainsAgentNames(t *testing.T) {
	prompt := BuildMultiAgentSystemPrompt("Claude Code", nil)
	for _, role := range []string{"planner", "executor", "verifier"} {
		if !strings.Contains(prompt, role) {
			t.Fatalf("prompt should contain %q", role)
		}
	}
}

// Multi-agent prompt contains JSON instruction format.
func TestBuildMultiAgentSystemPrompt_ContainsJSONInstruction(t *testing.T) {
	prompt := BuildMultiAgentSystemPrompt("Claude Code", nil)
	if !strings.Contains(prompt, `"agent"`) {
		t.Fatal("prompt should contain JSON agent key example")
	}
	if !strings.Contains(prompt, `"message"`) {
		t.Fatal("prompt should contain JSON message key example")
	}
}

// Multi-agent prompt contains TASK_COMPLETE instruction.
func TestBuildMultiAgentSystemPrompt_ContainsTaskComplete(t *testing.T) {
	prompt := BuildMultiAgentSystemPrompt("Claude Code", nil)
	if !strings.Contains(prompt, TaskCompleteMarker) {
		t.Fatal("prompt should contain TASK_COMPLETE marker")
	}
}

// Multi-agent prompt includes memory when provided.
func TestBuildMultiAgentSystemPrompt_WithMemories(t *testing.T) {
	memories := []string{"use gofmt", "no external deps"}
	prompt := BuildMultiAgentSystemPrompt("Claude Code", memories)
	if !strings.Contains(prompt, "Memory from previous sessions") {
		t.Fatal("prompt should include memory section")
	}
	for _, fact := range memories {
		if !strings.Contains(prompt, fact) {
			t.Fatalf("prompt should include fact %q", fact)
		}
	}
}

// Multi-agent prompt uses agentName, not hardcoded "Claude Code".
func TestBuildMultiAgentSystemPrompt_UsesAgentName(t *testing.T) {
	prompt := BuildMultiAgentSystemPrompt("Codex", nil)
	if !strings.Contains(prompt, "Codex") {
		t.Fatal("prompt should reference agent name 'Codex'")
	}
	if strings.Contains(prompt, "Claude Code") {
		t.Fatal("prompt should not contain 'Claude Code' when agent is 'Codex'")
	}
}

// Multi-agent prompt without memories has no memory section.
func TestBuildMultiAgentSystemPrompt_NoMemories(t *testing.T) {
	prompt := BuildMultiAgentSystemPrompt("Claude Code", nil)
	if strings.Contains(prompt, "Memory from previous sessions") {
		t.Fatal("prompt should not include memory section when no memories")
	}
}

// ---------------------------------------------------------------------------
// summarizeContext tests
// ---------------------------------------------------------------------------

// When below threshold, messages are returned unchanged.
func TestSummarizeContext_BelowThreshold(t *testing.T) {
	oldThreshold := ContextSummaryThreshold
	ContextSummaryThreshold = 100
	t.Cleanup(func() { ContextSummaryThreshold = oldThreshold })

	messages := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	result := summarizeContext(messages, "unused", "unused")
	if len(result) != len(messages) {
		t.Fatalf("expected %d messages, got %d", len(messages), len(result))
	}
}

// When above threshold, messages are compressed via LLM call.
func TestSummarizeContext_AboveThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := Response{
			ID:      "summary-id",
			Choices: []Choice{{Message: Message{Role: "assistant", Content: "This is a summary of the conversation."}}},
			Usage:   Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	oldThreshold := ContextSummaryThreshold
	ContextSummaryThreshold = 5
	t.Cleanup(func() { ContextSummaryThreshold = oldThreshold })

	// Create more messages than threshold.
	messages := make([]Message, 20)
	messages[0] = Message{Role: "system", Content: "system prompt"}
	for i := 1; i < 20; i++ {
		if i%2 == 1 {
			messages[i] = Message{Role: "user", Content: fmt.Sprintf("user msg %d", i)}
		} else {
			messages[i] = Message{Role: "assistant", Content: fmt.Sprintf("assistant msg %d", i)}
		}
	}

	result := summarizeContext(messages, "test-key", "test-model")
	// Should have: system prompt + summary + last 10 recent = 12
	if len(result) != 12 {
		t.Fatalf("expected 12 messages after summarization, got %d", len(result))
	}
	if result[0].Role != "system" {
		t.Fatal("first message should be system prompt")
	}
	if !strings.Contains(result[1].Content, "summary") {
		t.Fatalf("second message should contain summary, got: %s", result[1].Content)
	}
}

// API error during summarization returns original messages (graceful fallback).
func TestSummarizeContext_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		errResp := ErrorResponse{}
		errResp.Error.Message = "server error"
		json.NewEncoder(w).Encode(errResp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	oldThreshold := ContextSummaryThreshold
	ContextSummaryThreshold = 5
	t.Cleanup(func() { ContextSummaryThreshold = oldThreshold })

	messages := make([]Message, 20)
	messages[0] = Message{Role: "system", Content: "system prompt"}
	for i := 1; i < 20; i++ {
		messages[i] = Message{Role: "user", Content: fmt.Sprintf("msg %d", i)}
	}

	result := summarizeContext(messages, "test-key", "test-model")
	// Should return original messages unchanged on API error.
	if len(result) != len(messages) {
		t.Fatalf("expected %d messages (unchanged), got %d", len(messages), len(result))
	}
}

// System prompt contains the completion marker instruction.
func TestBuildSystemPrompt_ContainsMarker(t *testing.T) {
	prompt := BuildSystemPrompt("Claude Code", nil)
	if !strings.Contains(prompt, TaskCompleteMarker) {
		t.Fatal("system prompt should reference TASK_COMPLETE marker")
	}
}

// System prompt includes memory facts when provided.
func TestBuildSystemPrompt_WithMemories(t *testing.T) {
	memories := []string{"Go 1.23 with no external deps", "Tests must not use t.Parallel()"}
	prompt := BuildSystemPrompt("Claude Code", memories)
	if !strings.Contains(prompt, "Memory from previous sessions") {
		t.Fatal("prompt should include memory section header")
	}
	for _, fact := range memories {
		if !strings.Contains(prompt, fact) {
			t.Fatalf("prompt should include fact %q", fact)
		}
	}
}

// System prompt without memories has no memory section.
func TestBuildSystemPrompt_NoMemories(t *testing.T) {
	prompt := BuildSystemPrompt("Claude Code", nil)
	if strings.Contains(prompt, "Memory from previous sessions") {
		t.Fatal("prompt should not include memory section when no memories")
	}
}

// System prompt includes MEMORY_SAVE instruction.
func TestBuildSystemPrompt_MemorySaveInstruction(t *testing.T) {
	prompt := BuildSystemPrompt("Claude Code", nil)
	if !strings.Contains(prompt, "MEMORY_SAVE:") {
		t.Fatal("prompt should include MEMORY_SAVE instruction")
	}
}

// System prompt uses the provided agent name throughout.
func TestBuildSystemPrompt_UsesAgentName(t *testing.T) {
	prompt := BuildSystemPrompt("Codex", nil)
	if !strings.Contains(prompt, "Codex CLI") {
		t.Fatal("prompt should reference 'Codex CLI'")
	}
	if strings.Contains(prompt, "Claude Code") {
		t.Fatal("prompt should not contain 'Claude Code' when agent is Codex")
	}
}

// System prompt says "Claude Code CLI" when agent name is "Claude Code".
func TestBuildSystemPrompt_ClaudeCode(t *testing.T) {
	prompt := BuildSystemPrompt("Claude Code", nil)
	if !strings.Contains(prompt, "Claude Code CLI") {
		t.Fatal("prompt should reference 'Claude Code CLI'")
	}
}

// CallOpenRouter parses a well-formed response from a mock server.
func TestCallOpenRouter_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request structure.
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("expected model test-model, got %s", req.Model)
		}

		resp := Response{
			ID: "test-id",
			Choices: []Choice{
				{Message: Message{Role: "assistant", Content: "test reply"}, FinishReason: "stop"},
			},
			Usage: Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	msgs := []Message{{Role: "user", Content: "hello"}}
	reply, usage, err := CallOpenRouter("test-key", "test-model", msgs, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "test reply" {
		t.Fatalf("got reply %q, want %q", reply, "test reply")
	}
	if usage.TotalTokens != 15 {
		t.Fatalf("got total tokens %d, want 15", usage.TotalTokens)
	}
}

// CallOpenRouter returns a descriptive error on API error responses.
func TestCallOpenRouter_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		resp := ErrorResponse{}
		resp.Error.Message = "rate limited"
		resp.Error.Code = 429
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	msgs := []Message{{Role: "user", Content: "hello"}}
	_, _, err := CallOpenRouter("test-key", "test-model", msgs, 0.5)
	if err == nil {
		t.Fatal("expected error for 429 response")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error should mention rate limited: %v", err)
	}
}

// CallOpenRouter returns an error when the response has no choices.
func TestCallOpenRouter_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := Response{ID: "test-id", Choices: []Choice{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	msgs := []Message{{Role: "user", Content: "hello"}}
	_, _, err := CallOpenRouter("test-key", "test-model", msgs, 0.5)
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("error should mention empty choices: %v", err)
	}
}

// Verify request JSON structure sent to the API.
func TestCallOpenRouter_RequestStructure(t *testing.T) {
	var receivedReq Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedReq)
		resp := Response{
			ID:      "test-id",
			Choices: []Choice{{Message: Message{Role: "assistant", Content: "ok"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "usr"},
	}
	_, _, err := CallOpenRouter("key", "mymodel", msgs, 0.7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedReq.Model != "mymodel" {
		t.Fatalf("model: got %q want %q", receivedReq.Model, "mymodel")
	}
	if len(receivedReq.Messages) != 2 {
		t.Fatalf("messages count: got %d want 2", len(receivedReq.Messages))
	}
	if fmt.Sprintf("%.1f", receivedReq.Temperature) != "0.7" {
		t.Fatalf("temperature: got %v want 0.7", receivedReq.Temperature)
	}
}

// CallOpenRouter includes provider routing when Provider is set.
func TestCallOpenRouter_ProviderRouting(t *testing.T) {
	var receivedReq Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedReq)
		resp := Response{
			ID:      "test-id",
			Choices: []Choice{{Message: Message{Role: "assistant", Content: "ok"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	oldProvider := Provider
	Provider = "amazon-bedrock"
	t.Cleanup(func() { Provider = oldProvider })

	msgs := []Message{{Role: "user", Content: "hello"}}
	_, _, err := CallOpenRouter("key", "test-model", msgs, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedReq.Provider == nil {
		t.Fatal("expected provider field in request")
	}
	if len(receivedReq.Provider.Only) != 1 || receivedReq.Provider.Only[0] != "amazon-bedrock" {
		t.Fatalf("provider only: got %v, want [amazon-bedrock]", receivedReq.Provider.Only)
	}
}

// CallOpenRouter omits provider field when Provider is empty.
func TestCallOpenRouter_NoProviderWhenEmpty(t *testing.T) {
	var receivedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody)
		resp := Response{
			ID:      "test-id",
			Choices: []Choice{{Message: Message{Role: "assistant", Content: "ok"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := Endpoint
	Endpoint = srv.URL
	t.Cleanup(func() { Endpoint = oldEndpoint })

	oldProvider := Provider
	Provider = ""
	t.Cleanup(func() { Provider = oldProvider })

	msgs := []Message{{Role: "user", Content: "hello"}}
	_, _, err := CallOpenRouter("key", "test-model", msgs, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, exists := receivedBody["provider"]; exists {
		t.Fatal("provider field should be omitted when Provider is empty")
	}
}
