package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// When no custom socket is set, args pass through unchanged.
func TestTmuxArgs_DefaultSocket(t *testing.T) {
	tmuxSocket = ""
	got := tmuxArgs("has-session", "-t", "abc")
	if len(got) != 3 {
		t.Fatalf("unexpected args length: got %d", len(got))
	}
	if got[0] != "has-session" || got[2] != "abc" {
		t.Fatalf("unexpected args: %#v", got)
	}
}

// A custom socket prepends "-L <socket>" to the args.
func TestTmuxArgs_CustomSocket(t *testing.T) {
	tmuxSocket = "mysock"
	got := tmuxArgs("capture-pane", "-p")
	if len(got) != 4 {
		t.Fatalf("unexpected args length: got %d", len(got))
	}
	if got[0] != "-L" || got[1] != "mysock" || got[2] != "capture-pane" {
		t.Fatalf("unexpected args: %#v", got)
	}
}

// Env var prefixes are preserved and the binary is resolved to an absolute path.
func TestResolveStartupCommand(t *testing.T) {
	got, err := resolveStartupCommand("GT_ROLE=refinery go version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, " version") {
		t.Fatalf("unexpected command: %q", got)
	}
	if !strings.Contains(got, "/go") && !strings.Contains(got, "\\go") {
		t.Fatalf("expected absolute binary path in command, got %q", got)
	}
}

// KEY=VALUE tokens are detected; flags like --model=sonnet are not.
func TestIsShellEnvAssignment(t *testing.T) {
	if !isShellEnvAssignment("GT_ROLE=refinery") {
		t.Fatal("expected env assignment token")
	}
	if isShellEnvAssignment("--model=sonnet") {
		t.Fatal("flags should not be treated as env assignment")
	}
}

// Recovery is triggered for server/session loss but not for unrelated errors.
func TestShouldRecoverSession(t *testing.T) {
	if !shouldRecoverSession(errors.New("no server running on /tmp/tmux")) {
		t.Fatal("expected recovery for server loss")
	}
	if shouldRecoverSession(errors.New("permission denied opening file")) {
		t.Fatal("did not expect recovery for unrelated errors")
	}
}

// A dead pane line (dead=1) is parsed into dead=true, the exit status, and the command name.
func TestParsePaneStateLine_Dead(t *testing.T) {
	dead, status, current, err := parsePaneStateLine("1\t127\tclaude\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dead || status != 127 || current != "claude" {
		t.Fatalf("unexpected parse: dead=%v status=%d current=%q", dead, status, current)
	}
}

// A live pane line (dead=0) with an empty exit status parses as dead=false, status=0.
func TestParsePaneStateLine_Alive(t *testing.T) {
	dead, status, current, err := parsePaneStateLine("0\t\tbash\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dead || status != 0 || current != "bash" {
		t.Fatalf("unexpected parse: dead=%v status=%d current=%q", dead, status, current)
	}
}

// Malformed input returns a parse error.
func TestParsePaneStateLine_Invalid(t *testing.T) {
	_, _, _, err := parsePaneStateLine("bad-line")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// overrideTimers mutates package-level globals. Tests that call this
// must NOT use t.Parallel() — doing so would cause data races.
func overrideTimers(t *testing.T) {
	t.Helper()
	oldPoll := pollInterval
	oldStable := stableWindow
	pollInterval = 1 * time.Millisecond
	stableWindow = 3 * time.Millisecond
	t.Cleanup(func() {
		pollInterval = oldPoll
		stableWindow = oldStable
	})
}

// Once pane content changes and holds steady for the stable window, the new content is returned.
func TestWaitForPaneUpdateWithCapture_ReturnsStablePane(t *testing.T) {
	overrideTimers(t)

	panes := []string{"same", "new", "new", "new", "new"}
	i := 0
	capture := func() (string, error) {
		if i >= len(panes) {
			return panes[len(panes)-1], nil
		}
		v := panes[i]
		i++
		return v, nil
	}

	alwaysAlive := func() (bool, error) { return true, nil }
	got, err := waitForPaneUpdateWithCapture("same", 100*time.Millisecond, capture, alwaysAlive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "new" {
		t.Fatalf("got %q, want new", got)
	}
}

// Times out with an error when the pane never changes from the previous value.
func TestWaitForPaneUpdateWithCapture_TimeoutNoChanges(t *testing.T) {
	overrideTimers(t)

	capture := func() (string, error) {
		return "same", nil
	}

	alwaysAlive := func() (bool, error) { return true, nil }
	got, err := waitForPaneUpdateWithCapture("same", 5*time.Millisecond, capture, alwaysAlive)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "still working") {
		t.Fatalf("expected 'still working' error, got: %v", err)
	}
	if got != "same" {
		t.Fatalf("got %q, want same", got)
	}
}

// Capture function errors propagate immediately without retrying.
func TestWaitForPaneUpdateWithCapture_CaptureError(t *testing.T) {
	capture := func() (string, error) {
		return "", errors.New("boom")
	}

	alwaysAlive := func() (bool, error) { return true, nil }
	_, err := waitForPaneUpdateWithCapture("same", 50*time.Millisecond, capture, alwaysAlive)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "boom" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// When pane never changes and the process is dead, the error reports process death.
func TestWaitForPaneUpdateWithCapture_TimeoutProcessDead(t *testing.T) {
	overrideTimers(t)

	capture := func() (string, error) {
		return "same", nil
	}
	alwaysDead := func() (bool, error) { return false, nil }

	got, err := waitForPaneUpdateWithCapture("same", 5*time.Millisecond, capture, alwaysDead)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "process is dead") {
		t.Fatalf("expected 'process is dead' error, got: %v", err)
	}
	if got != "same" {
		t.Fatalf("got %q, want same", got)
	}
}

// ---------------------------------------------------------------------------
// Autonomous mode unit tests
// ---------------------------------------------------------------------------

// ANSI escape sequences and excessive blank lines are stripped.
func TestCleanPaneOutput_StripsANSI(t *testing.T) {
	input := "\x1b[32mHello\x1b[0m World\x1b[1m!\x1b[0m"
	got := cleanPaneOutput(input)
	if got != "Hello World!" {
		t.Fatalf("got %q, want %q", got, "Hello World!")
	}
}

// Runs of 3+ blank lines are collapsed to 2.
func TestCleanPaneOutput_CollapsesBlankLines(t *testing.T) {
	input := "line1\n\n\n\n\nline2"
	got := cleanPaneOutput(input)
	if !strings.Contains(got, "line1\n\nline2") {
		t.Fatalf("blank lines not collapsed: %q", got)
	}
}

// Trailing whitespace per line is removed.
func TestCleanPaneOutput_TrimsTrailingWhitespace(t *testing.T) {
	input := "hello   \nworld\t\t\n"
	got := cleanPaneOutput(input)
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t") != line {
			t.Fatalf("line %d has trailing whitespace: %q", i, line)
		}
	}
}

// Short strings pass through unchanged.
func TestTruncateForLog_Short(t *testing.T) {
	got := truncateForLog("hello", 10)
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

// Long strings are truncated with "..." suffix.
func TestTruncateForLog_Long(t *testing.T) {
	got := truncateForLog("hello world", 8)
	if got != "hello..." {
		t.Fatalf("got %q, want %q", got, "hello...")
	}
}

// Very small maxLen doesn't panic.
func TestTruncateForLog_TinyMax(t *testing.T) {
	got := truncateForLog("hello", 2)
	if got != "he" {
		t.Fatalf("got %q, want %q", got, "he")
	}
}

// System prompt contains the completion marker instruction.
func TestBuildSystemPrompt_ContainsMarker(t *testing.T) {
	prompt := buildSystemPrompt()
	if !strings.Contains(prompt, taskCompleteMarker) {
		t.Fatal("system prompt should reference TASK_COMPLETE marker")
	}
}

// callOpenRouter parses a well-formed response from a mock server.
func TestCallOpenRouter_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request structure.
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		var req openRouterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("expected model test-model, got %s", req.Model)
		}

		resp := openRouterResponse{
			ID: "test-id",
			Choices: []openRouterChoice{
				{Message: openRouterMessage{Role: "assistant", Content: "test reply"}, FinishReason: "stop"},
			},
			Usage: openRouterUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{{Role: "user", Content: "hello"}}
	reply, usage, err := callOpenRouter("test-key", "test-model", msgs, 0.5)
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

// callOpenRouter returns a descriptive error on API error responses.
func TestCallOpenRouter_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		resp := openRouterError{}
		resp.Error.Message = "rate limited"
		resp.Error.Code = 429
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{{Role: "user", Content: "hello"}}
	_, _, err := callOpenRouter("test-key", "test-model", msgs, 0.5)
	if err == nil {
		t.Fatal("expected error for 429 response")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error should mention rate limited: %v", err)
	}
}

// callOpenRouter returns an error when the response has no choices.
func TestCallOpenRouter_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openRouterResponse{ID: "test-id", Choices: []openRouterChoice{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{{Role: "user", Content: "hello"}}
	_, _, err := callOpenRouter("test-key", "test-model", msgs, 0.5)
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("error should mention empty choices: %v", err)
	}
}

// Verify request JSON structure sent to the API.
func TestCallOpenRouter_RequestStructure(t *testing.T) {
	var receivedReq openRouterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedReq)
		resp := openRouterResponse{
			ID:      "test-id",
			Choices: []openRouterChoice{{Message: openRouterMessage{Role: "assistant", Content: "ok"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "usr"},
	}
	_, _, err := callOpenRouter("key", "mymodel", msgs, 0.7)
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
