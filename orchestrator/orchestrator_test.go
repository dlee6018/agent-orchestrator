package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// System prompt contains the completion marker instruction.
func TestBuildSystemPrompt_ContainsMarker(t *testing.T) {
	prompt := BuildSystemPrompt(nil)
	if !strings.Contains(prompt, TaskCompleteMarker) {
		t.Fatal("system prompt should reference TASK_COMPLETE marker")
	}
}

// System prompt includes memory facts when provided.
func TestBuildSystemPrompt_WithMemories(t *testing.T) {
	memories := []string{"Go 1.23 with no external deps", "Tests must not use t.Parallel()"}
	prompt := BuildSystemPrompt(memories)
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
	prompt := BuildSystemPrompt(nil)
	if strings.Contains(prompt, "Memory from previous sessions") {
		t.Fatal("prompt should not include memory section when no memories")
	}
}

// System prompt includes MEMORY_SAVE instruction.
func TestBuildSystemPrompt_MemorySaveInstruction(t *testing.T) {
	prompt := BuildSystemPrompt(nil)
	if !strings.Contains(prompt, "MEMORY_SAVE:") {
		t.Fatal("prompt should include MEMORY_SAVE instruction")
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
