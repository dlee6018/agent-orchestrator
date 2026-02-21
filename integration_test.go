//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dlee6018/agent-orchestrator/dashboard"
	"github.com/dlee6018/agent-orchestrator/memory"
	"github.com/dlee6018/agent-orchestrator/orchestrator"
	"github.com/dlee6018/agent-orchestrator/tmux"
)

// setupIntegration prepares an isolated tmux environment for a single test.
// It creates a dedicated tmux server via a unique socket so tests never touch
// the user's real tmux.  Tests must NOT call t.Parallel — they mutate
// package-level globals (tmux.Socket, tmux.PollInterval, etc.).
func setupIntegration(t *testing.T) (session, workDir, command string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available, skipping integration test")
	}

	origPoll := tmux.PollInterval
	origStable := tmux.StableWindow
	origSettle := tmux.StartupSettleWindow
	origSocket := tmux.Socket

	socket := fmt.Sprintf("go-orch-inttest-%d", time.Now().UnixNano())
	session = fmt.Sprintf("inttest-%d", time.Now().UnixNano())
	workDir = t.TempDir()
	command = "bash"

	tmux.Socket = socket
	tmux.PollInterval = 50 * time.Millisecond
	tmux.StableWindow = 500 * time.Millisecond
	tmux.StartupSettleWindow = 300 * time.Millisecond

	t.Cleanup(func() {
		exec.Command("tmux", "-L", socket, "kill-server").Run()
		tmux.Socket = origSocket
		tmux.PollInterval = origPoll
		tmux.StableWindow = origStable
		tmux.StartupSettleWindow = origSettle
	})

	return session, workDir, command
}

// createTestSession is a test helper that calls EnsureClaudeSession and lets
// bash settle before returning.
func createTestSession(t *testing.T, session, workDir, command string) {
	t.Helper()
	if err := tmux.EnsureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("EnsureClaudeSession: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
}

// setupAutonomous configures orchestrator.Endpoint and orchestrator.MaxIterations for autonomous
// loop tests, restoring them on cleanup.
func setupAutonomous(t *testing.T, endpoint string, max int) {
	t.Helper()
	oldEndpoint := orchestrator.Endpoint
	oldMax := orchestrator.MaxIterations
	orchestrator.Endpoint = endpoint
	orchestrator.MaxIterations = max
	t.Cleanup(func() {
		orchestrator.Endpoint = oldEndpoint
		orchestrator.MaxIterations = oldMax
	})
}

// mockOpenRouter creates an httptest server that calls handler for each request.
// Returns the server (caller should defer srv.Close()).
func mockOpenRouter(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// respondJSON writes an orchestrator.Response as JSON.
func respondJSON(w http.ResponseWriter, reply string, callID int) {
	resp := orchestrator.Response{
		ID: fmt.Sprintf("call-%d", callID),
		Choices: []orchestrator.Choice{
			{Message: orchestrator.Message{Role: "assistant", Content: reply}, FinishReason: "stop"},
		},
		Usage: orchestrator.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// Tmux integration tests
// ---------------------------------------------------------------------------

// Returns false for a session that was never created.
func TestIntegration_TmuxHasSession_NonExistent(t *testing.T) {
	session, _, _ := setupIntegration(t)

	err := tmux.RunTmux("has-session", "-t", session)
	if err == nil {
		t.Fatal("session should not exist before creation")
	}
}

// A real tmux session is created from scratch when none exists.
func TestIntegration_EnsureClaudeSession_CreatesSession(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	if err := tmux.EnsureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("EnsureClaudeSession: %v", err)
	}

	err := tmux.RunTmux("has-session", "-t", session)
	if err != nil {
		t.Fatal("session should exist after EnsureClaudeSession")
	}
}

// Calling EnsureClaudeSession twice is safe — the session survives the second call.
func TestIntegration_EnsureClaudeSession_Idempotent(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	if err := tmux.EnsureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := tmux.EnsureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}

	err := tmux.RunTmux("has-session", "-t", session)
	if err != nil {
		t.Fatal("session should still exist after duplicate ensure")
	}
}

// Capturing a live pane returns non-empty output.
func TestIntegration_CapturePane(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if len(pane) == 0 {
		t.Fatal("expected non-empty pane capture")
	}
}

// Sending an echo command via SendMessage makes the marker appear in the captured pane.
func TestIntegration_SendMessage_AppearsInPane(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	marker := fmt.Sprintf("SENDTEST_%d", time.Now().UnixNano())
	if err := tmux.SendMessage(session, fmt.Sprintf("echo %s", marker)); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	time.Sleep(1 * time.Second)

	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not found in pane:\n%s", marker, pane)
	}
}

// Polling detects new pane content after sending a command and returns once stable.
func TestIntegration_WaitForPaneUpdate(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	initial, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("initial capture: %v", err)
	}

	marker := fmt.Sprintf("PANEUPD_%d", time.Now().UnixNano())
	if err := tmux.SendMessage(session, fmt.Sprintf("echo %s", marker)); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	pane, err := tmux.WaitForPaneUpdate(session, initial, 10*time.Second)
	if err != nil {
		t.Fatalf("WaitForPaneUpdate: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in updated pane:\n%s", marker, pane)
	}
}

// Normal send-and-capture succeeds without needing recovery.
func TestIntegration_SendAndCaptureWithRecovery_HappyPath(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	initial, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("initial capture: %v", err)
	}

	marker := fmt.Sprintf("HAPPY_%d", time.Now().UnixNano())
	pane, err := tmux.SendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), initial)
	if err != nil {
		t.Fatalf("SendAndCaptureWithRecovery: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in output:\n%s", marker, pane)
	}
}

// After the session is killed, recovery recreates it and the message still succeeds.
func TestIntegration_SendAndCaptureWithRecovery_AfterSessionKill(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	// Simulate session crash.
	if err := tmux.RunTmux("kill-session", "-t", session); err != nil {
		t.Fatalf("kill-session: %v", err)
	}

	// sendAndCaptureWithRecovery should recreate the session and succeed.
	marker := fmt.Sprintf("RECOVER_%d", time.Now().UnixNano())
	pane, err := tmux.SendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), "")
	if err != nil {
		t.Fatalf("SendAndCaptureWithRecovery after kill: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in recovered output:\n%s", marker, pane)
	}
}

// After the entire tmux server is killed, recovery spins up a new server and session.
func TestIntegration_SendAndCaptureWithRecovery_AfterServerKill(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	// Kill the entire tmux server — more severe than killing one session.
	exec.Command("tmux", "-L", tmux.Socket, "kill-server").Run()
	time.Sleep(200 * time.Millisecond)

	marker := fmt.Sprintf("SRVRECOV_%d", time.Now().UnixNano())
	pane, err := tmux.SendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), "")
	if err != nil {
		t.Fatalf("SendAndCaptureWithRecovery after server kill: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in output after server recovery:\n%s", marker, pane)
	}
}

// Three sequential messages each appear in the pane, testing lastPane tracking across turns.
func TestIntegration_MultipleMessages(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	lastPane := ""
	for i := 0; i < 3; i++ {
		marker := fmt.Sprintf("MSG%d_%d", i, time.Now().UnixNano())
		pane, err := tmux.SendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), lastPane)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !strings.Contains(pane, marker) {
			t.Fatalf("message %d: marker %q not in pane:\n%s", i, marker, pane)
		}
		lastPane = pane
	}
}

// CleanupSession removes the tmux session so it no longer exists.
func TestIntegration_CleanupSession(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	tmux.CleanupSession(session)

	err := tmux.RunTmux("has-session", "-t", session)
	if err == nil {
		t.Fatal("session should not exist after CleanupSession")
	}
}

// CleanPaneOutput strips ANSI sequences from real tmux pane captures.
func TestIntegration_CleanPaneOutput_RealPane(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	marker := fmt.Sprintf("CLEAN_%d", time.Now().UnixNano())

	// Use printf with ANSI color codes to generate colored output.
	colorCmd := fmt.Sprintf(`printf '\033[31m%s\033[0m\n'`, marker)
	if err := tmux.SendMessage(session, colorCmd); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	time.Sleep(1 * time.Second)

	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}

	cleaned := tmux.CleanPaneOutput(pane)
	if !strings.Contains(cleaned, marker) {
		t.Fatalf("marker %q not in cleaned output:\n%s", marker, cleaned)
	}
	// The raw ANSI escape should be gone.
	if strings.Contains(cleaned, "\x1b[") {
		t.Fatalf("cleaned output still contains ANSI escapes:\n%s", cleaned)
	}
}

// ---------------------------------------------------------------------------
// Autonomous loop integration tests
// ---------------------------------------------------------------------------

// The autonomous loop sends commands from a mock LLM to a real tmux session,
// and terminates when the mock responds with TASK_COMPLETE.
func TestIntegration_AutonomousLoop_CompletesTask(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	callCount := 0
	marker := fmt.Sprintf("AUTO_%d", time.Now().UnixNano())

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "echo a marker", "Claude Code", nil, nil)

	if callCount < 2 {
		t.Fatalf("expected at least 2 API calls, got %d", callCount)
	}

	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in pane after autonomous loop:\n%s", marker, pane)
	}
}

// The autonomous loop respects the max iteration limit.
func TestIntegration_AutonomousLoop_MaxIterations(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	callCount := 0
	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		respondJSON(w, "echo iteration", callCount)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 3)
	createTestSession(t, session, workDir, command)

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "never-ending task", "Claude Code", nil, nil)

	if callCount != 3 {
		t.Fatalf("expected exactly 3 API calls (maxIterations=3), got %d", callCount)
	}
}

// The autonomous loop passes Claude Code output back to the LLM in conversation history.
func TestIntegration_AutonomousLoop_FeedbackLoop(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("FEEDBACK_%d", time.Now().UnixNano())
	callCount := 0
	var secondCallMessages []orchestrator.Message

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req orchestrator.Request
		json.NewDecoder(r.Body).Decode(&req)
		callCount++
		if callCount == 2 {
			secondCallMessages = req.Messages
		}
		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "echo test", "Claude Code", nil, nil)

	if len(secondCallMessages) == 0 {
		t.Fatal("second call messages not captured")
	}

	found := false
	for _, msg := range secondCallMessages {
		if msg.Role == "user" && strings.Contains(msg.Content, marker) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("marker %q not found in second API call messages; LLM is not receiving Claude Code feedback", marker)
	}
}

// Conversation history contains system prompt, task description, and correct role alternation.
func TestIntegration_AutonomousLoop_ConversationHistoryStructure(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker1 := fmt.Sprintf("HIST1_%d", time.Now().UnixNano())
	marker2 := fmt.Sprintf("HIST2_%d", time.Now().UnixNano())
	callCount := 0
	var allCapturedMessages [][]orchestrator.Message

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req orchestrator.Request
		json.NewDecoder(r.Body).Decode(&req)
		callCount++
		// Snapshot the messages array for each call.
		snapshot := make([]orchestrator.Message, len(req.Messages))
		copy(snapshot, req.Messages)
		allCapturedMessages = append(allCapturedMessages, snapshot)

		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker1), callCount)
		case 2:
			respondJSON(w, fmt.Sprintf("echo %s", marker2), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "run two commands", "Claude Code", nil, nil)

	if len(allCapturedMessages) < 3 {
		t.Fatalf("expected at least 3 API calls, got %d", len(allCapturedMessages))
	}

	// --- Call 1: should have system + user (task description) ---
	call1 := allCapturedMessages[0]
	if len(call1) != 2 {
		t.Fatalf("call 1: expected 2 messages (system + user), got %d", len(call1))
	}
	if call1[0].Role != "system" {
		t.Fatalf("call 1: first message should be system, got %q", call1[0].Role)
	}
	if !strings.Contains(call1[0].Content, orchestrator.TaskCompleteMarker) {
		t.Fatal("call 1: system prompt missing TASK_COMPLETE instruction")
	}
	if call1[1].Role != "user" {
		t.Fatalf("call 1: second message should be user, got %q", call1[1].Role)
	}
	if !strings.Contains(call1[1].Content, "run two commands") {
		t.Fatal("call 1: user message missing task description")
	}

	// --- Call 2: system + user + assistant (reply 1) + user (pane output) ---
	call2 := allCapturedMessages[1]
	if len(call2) != 4 {
		t.Fatalf("call 2: expected 4 messages, got %d", len(call2))
	}
	if call2[2].Role != "assistant" {
		t.Fatalf("call 2: third message should be assistant, got %q", call2[2].Role)
	}
	if !strings.Contains(call2[2].Content, marker1) {
		t.Fatalf("call 2: assistant message should contain marker1 %q", marker1)
	}
	if call2[3].Role != "user" {
		t.Fatalf("call 2: fourth message should be user, got %q", call2[3].Role)
	}
	if !strings.Contains(call2[3].Content, "Claude Code output:") {
		t.Fatal("call 2: user feedback message missing 'Claude Code output:' prefix (agentName=Claude Code)")
	}

	// --- Call 3: history grows by 2 more (assistant + user for iteration 2) ---
	call3 := allCapturedMessages[2]
	if len(call3) != 6 {
		t.Fatalf("call 3: expected 6 messages, got %d", len(call3))
	}
	if call3[4].Role != "assistant" {
		t.Fatalf("call 3: fifth message should be assistant, got %q", call3[4].Role)
	}
	if !strings.Contains(call3[4].Content, marker2) {
		t.Fatalf("call 3: assistant message should contain marker2 %q", marker2)
	}
	if call3[5].Role != "user" {
		t.Fatalf("call 3: sixth message should be user, got %q", call3[5].Role)
	}

	// Verify role alternation: system, user, (assistant, user)* throughout.
	for i := 2; i < len(call3); i += 2 {
		if call3[i].Role != "assistant" {
			t.Fatalf("call 3: message %d should be assistant, got %q", i, call3[i].Role)
		}
		if call3[i+1].Role != "user" {
			t.Fatalf("call 3: message %d should be user, got %q", i+1, call3[i+1].Role)
		}
	}
}

// TASK_COMPLETE detected even when embedded in a longer response.
func TestIntegration_AutonomousLoop_TaskCompleteEmbedded(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	callCount := 0
	marker := fmt.Sprintf("EMBED_%d", time.Now().UnixNano())

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			// TASK_COMPLETE embedded in surrounding text.
			respondJSON(w, "I have verified everything works.\nTASK_COMPLETE\nAll done.", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "embedded completion test", "Claude Code", nil, nil)

	// Should have completed after 2 iterations (not run to maxIterations).
	if callCount != 2 {
		t.Fatalf("expected 2 API calls (TASK_COMPLETE embedded in call 2), got %d", callCount)
	}

	// Verify the first command actually ran.
	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in pane:\n%s", marker, pane)
	}
}

// Three consecutive API errors cause the autonomous loop to abort.
func TestIntegration_AutonomousLoop_APIErrorAbort(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	var apiCalls int32
	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiCalls, 1)
		// Always return an error.
		w.WriteHeader(http.StatusInternalServerError)
		errResp := orchestrator.ErrorResponse{}
		errResp.Error.Message = "server on fire"
		errResp.Error.Code = 500
		json.NewEncoder(w).Encode(errResp)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)

	start := time.Now()
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "doomed task", "Claude Code", nil, nil)
	elapsed := time.Since(start)

	calls := int(atomic.LoadInt32(&apiCalls))
	if calls != 3 {
		t.Fatalf("expected exactly 3 API calls before abort, got %d", calls)
	}
	// Should have waited ~10s (two 5s sleeps between the 3 attempts).
	if elapsed < 8*time.Second {
		t.Fatalf("expected at least ~10s of backoff, but only took %s", elapsed)
	}
}

// A transient API error followed by success does not abort.
func TestIntegration_AutonomousLoop_APIErrorRecovery(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("RECOVER_%d", time.Now().UnixNano())
	var apiCalls int32

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		call := int(atomic.AddInt32(&apiCalls, 1))
		switch call {
		case 1:
			// First call: transient error.
			w.WriteHeader(http.StatusServiceUnavailable)
			errResp := orchestrator.ErrorResponse{}
			errResp.Error.Message = "temporarily unavailable"
			errResp.Error.Code = 503
			json.NewEncoder(w).Encode(errResp)
		case 2:
			// Second call (retry): success — send a command.
			respondJSON(w, fmt.Sprintf("echo %s", marker), call)
		default:
			// Third call: complete.
			respondJSON(w, "TASK_COMPLETE", call)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "transient error task", "Claude Code", nil, nil)

	calls := int(atomic.LoadInt32(&apiCalls))
	if calls != 3 {
		t.Fatalf("expected 3 API calls (1 error + 1 command + 1 complete), got %d", calls)
	}

	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in pane after recovery:\n%s", marker, pane)
	}
}

// Multi-step task: LLM creates a file, reads it back, verifies content.
func TestIntegration_AutonomousLoop_MultiStepTask(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("MULTI_%d", time.Now().UnixNano())
	callCount := 0
	var capturedMessages [][]orchestrator.Message

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req orchestrator.Request
		json.NewDecoder(r.Body).Decode(&req)
		callCount++
		snapshot := make([]orchestrator.Message, len(req.Messages))
		copy(snapshot, req.Messages)
		capturedMessages = append(capturedMessages, snapshot)

		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo '%s' > testfile.txt", marker), callCount)
		case 2:
			respondJSON(w, "cat testfile.txt", callCount)
		case 3:
			respondJSON(w, "TASK_COMPLETE", callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "create and verify file", "Claude Code", nil, nil)

	if callCount != 3 {
		t.Fatalf("expected 3 API calls for multi-step task, got %d", callCount)
	}

	if len(capturedMessages) < 3 {
		t.Fatalf("expected at least 3 captured message snapshots, got %d", len(capturedMessages))
	}
	call3 := capturedMessages[2]
	foundMarkerInHistory := false
	for _, msg := range call3 {
		if msg.Role == "user" && strings.Contains(msg.Content, marker) {
			foundMarkerInHistory = true
			break
		}
	}
	if !foundMarkerInHistory {
		t.Fatalf("marker %q from 'cat testfile.txt' not found in call 3 conversation history", marker)
	}
}

// Tmux session killed mid-loop: the error is fed back to the LLM.
func TestIntegration_AutonomousLoop_TmuxRecoveryMidLoop(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("TMXRECOV_%d", time.Now().UnixNano())
	callCount := 0

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, "echo before_kill", callCount)
		case 2:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)

	go func() {
		time.Sleep(1 * time.Second)
		_ = tmux.RunTmux("kill-session", "-t", session)
	}()

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "survive tmux kill", "Claude Code", nil, nil)

	if callCount < 2 {
		t.Fatalf("expected at least 2 API calls, got %d", callCount)
	}

	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane after recovery: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in pane after tmux recovery:\n%s", marker, pane)
	}
}

// API key and model are passed correctly in the HTTP request.
func TestIntegration_AutonomousLoop_APIKeyAndModel(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	var receivedAuth string
	var receivedModel string

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		var req orchestrator.Request
		json.NewDecoder(r.Body).Decode(&req)
		receivedModel = req.Model
		respondJSON(w, "TASK_COMPLETE", 1)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "sk-test-key-123", "anthropic/claude-sonnet-4", "api key test", "Claude Code", nil, nil)

	if receivedAuth != "Bearer sk-test-key-123" {
		t.Fatalf("expected auth header %q, got %q", "Bearer sk-test-key-123", receivedAuth)
	}
	if receivedModel != "anthropic/claude-sonnet-4" {
		t.Fatalf("expected model %q, got %q", "anthropic/claude-sonnet-4", receivedModel)
	}
}

// Immediate TASK_COMPLETE on iteration 1 exits without sending anything to tmux.
func TestIntegration_AutonomousLoop_ImmediateComplete(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	callCount := 0
	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		respondJSON(w, "TASK_COMPLETE", callCount)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	initial, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("initial capture: %v", err)
	}

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "instant done", "Claude Code", nil, nil)

	if callCount != 1 {
		t.Fatalf("expected exactly 1 API call, got %d", callCount)
	}

	after, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("after capture: %v", err)
	}
	if initial != after {
		t.Fatal("pane should not have changed when LLM immediately returned TASK_COMPLETE")
	}
}

// ---------------------------------------------------------------------------
// Dashboard SSE integration tests
// ---------------------------------------------------------------------------

// The autonomous loop emits the expected SSE events.
func TestIntegration_AutonomousLoop_EmitsSSEEvents(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("SSE_%d", time.Now().UnixNano())
	callCount := 0

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	broker := dashboard.NewSSEBroker()
	ch, unsub := broker.Subscribe()
	defer unsub()

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "emit SSE events", "Claude Code", broker, nil)

	var events []dashboard.IterationEvent
	for {
		select {
		case msg := <-ch:
			payload := strings.TrimPrefix(strings.TrimSpace(msg), "data: ")
			var evt dashboard.IterationEvent
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				events = append(events, evt)
			}
		default:
			goto collected
		}
	}
collected:

	if len(events) < 4 {
		t.Fatalf("expected at least 4 SSE events, got %d", len(events))
	}

	if events[0].Type != "task_info" {
		t.Fatalf("event 0: expected task_info, got %q", events[0].Type)
	}
	if events[0].Task != "emit SSE events" {
		t.Fatalf("task_info: expected task %q, got %q", "emit SSE events", events[0].Task)
	}

	typeSet := make(map[string]bool)
	for _, evt := range events {
		typeSet[evt.Type] = true
	}
	for _, want := range []string{"task_info", "iteration_start", "iteration_end", "complete"} {
		if !typeSet[want] {
			t.Fatalf("missing event type %q; got types: %v", want, typeSet)
		}
	}

	last := events[len(events)-1]
	if last.Type != "complete" {
		t.Fatalf("last event: expected complete, got %q", last.Type)
	}
	if last.Error != "" {
		t.Fatalf("last event: unexpected error: %s", last.Error)
	}

	for _, evt := range events {
		if evt.Type == "iteration_end" && evt.Tokens == nil {
			t.Fatalf("iteration_end event %d missing tokens", evt.Iteration)
		}
	}
}

// SSE events include the orchestrator message and Claude Code output.
func TestIntegration_AutonomousLoop_SSEEventContent(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("SSECONTENT_%d", time.Now().UnixNano())
	callCount := 0

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	broker := dashboard.NewSSEBroker()
	ch, unsub := broker.Subscribe()
	defer unsub()

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "content test", "Claude Code", broker, nil)

	var iterEndEvents []dashboard.IterationEvent
	for {
		select {
		case msg := <-ch:
			payload := strings.TrimPrefix(strings.TrimSpace(msg), "data: ")
			var evt dashboard.IterationEvent
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				if evt.Type == "iteration_end" {
					iterEndEvents = append(iterEndEvents, evt)
				}
			}
		default:
			goto done
		}
	}
done:

	if len(iterEndEvents) == 0 {
		t.Fatal("no iteration_end events received")
	}

	first := iterEndEvents[0]
	if !strings.Contains(first.Orchestrator, marker) {
		t.Fatalf("iteration_end orchestrator message missing marker %q: %q", marker, first.Orchestrator)
	}
	if !strings.Contains(first.ClaudeOutput, marker) {
		t.Fatalf("iteration_end claude_output missing marker %q: %q", marker, first.ClaudeOutput)
	}
	if first.DurationMs <= 0 {
		t.Fatalf("iteration_end duration_ms should be positive, got %d", first.DurationMs)
	}
}

// Max iterations reached emits a complete event with an error message.
func TestIntegration_AutonomousLoop_SSEMaxIterations(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, "echo looping", 1)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 2)
	createTestSession(t, session, workDir, command)

	broker := dashboard.NewSSEBroker()
	ch, unsub := broker.Subscribe()
	defer unsub()

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "max iter test", "Claude Code", broker, nil)

	var completeEvent *dashboard.IterationEvent
	for {
		select {
		case msg := <-ch:
			payload := strings.TrimPrefix(strings.TrimSpace(msg), "data: ")
			var evt dashboard.IterationEvent
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				if evt.Type == "complete" {
					completeEvent = &evt
				}
			}
		default:
			goto found
		}
	}
found:

	if completeEvent == nil {
		t.Fatal("no complete event received")
	}
	if completeEvent.Error == "" {
		t.Fatal("complete event should have an error for max iterations")
	}
	if !strings.Contains(completeEvent.Error, "maximum iterations") {
		t.Fatalf("complete event error should mention max iterations: %q", completeEvent.Error)
	}
}

// ---------------------------------------------------------------------------
// Persistent memory integration tests
// ---------------------------------------------------------------------------

// Memory facts are saved to memory.json after TASK_COMPLETE.
func TestIntegration_AutonomousLoop_MemorySaved(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	callCount := 0
	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, "echo hello\nMEMORY_SAVE: project uses bash for tests", callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "save memory test", "Claude Code", nil, nil)

	memPath := filepath.Join(workDir, "memory.json")
	data, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("memory.json should exist after TASK_COMPLETE: %v", err)
	}
	var facts []string
	if err := json.Unmarshal(data, &facts); err != nil {
		t.Fatalf("memory.json should be valid JSON: %v", err)
	}
	if len(facts) != 1 || facts[0] != "project uses bash for tests" {
		t.Fatalf("unexpected memory facts: %v", facts)
	}
}

// Memory is not saved when no MEMORY_SAVE lines are emitted.
func TestIntegration_AutonomousLoop_NoMemoryFileWhenEmpty(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		respondJSON(w, "TASK_COMPLETE", 1)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "no memory test", "Claude Code", nil, nil)

	memPath := filepath.Join(workDir, "memory.json")
	if _, err := os.Stat(memPath); !os.IsNotExist(err) {
		t.Fatal("memory.json should not be created when no facts exist")
	}
}

// Pre-existing memories are loaded into the system prompt.
func TestIntegration_AutonomousLoop_MemoryInjectedIntoPrompt(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	initialFacts := []string{"always use gofmt", "tests must not use t.Parallel()"}
	if err := memory.SaveMemory(workDir, initialFacts); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	var capturedSystemPrompt string
	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req orchestrator.Request
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
			capturedSystemPrompt = req.Messages[0].Content
		}
		respondJSON(w, "TASK_COMPLETE", 1)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	memories, err := memory.LoadMemory(workDir)
	if err != nil {
		t.Fatalf("LoadMemory: %v", err)
	}

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "memory prompt test", "Claude Code", nil, memories)

	if !strings.Contains(capturedSystemPrompt, "Memory from previous sessions") {
		t.Fatal("system prompt should include memory section header")
	}
	for _, fact := range initialFacts {
		if !strings.Contains(capturedSystemPrompt, fact) {
			t.Fatalf("system prompt missing fact %q", fact)
		}
	}
}

// MEMORY_SAVE lines are stripped before the reply is forwarded to Claude Code.
func TestIntegration_AutonomousLoop_MemorySaveStrippedFromReply(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("MEMSTRIP_%d", time.Now().UnixNano())
	callCount := 0

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s\nMEMORY_SAVE: secret fact", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "strip test", "Claude Code", nil, nil)

	pane, err := tmux.CapturePane(session)
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q should appear in pane output:\n%s", marker, pane)
	}
	if strings.Contains(pane, "MEMORY_SAVE") {
		t.Fatalf("MEMORY_SAVE should be stripped before sending to Claude Code:\n%s", pane)
	}
}

// Memory persists across the loop exit even when max iterations is reached.
func TestIntegration_AutonomousLoop_MemorySavedOnMaxIterations(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	callCount := 0
	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		respondJSON(w, fmt.Sprintf("echo iter %d\nMEMORY_SAVE: fact from iter %d", callCount, callCount), callCount)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 2)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "max iter memory", "Claude Code", nil, nil)

	facts, err := memory.LoadMemory(workDir)
	if err != nil {
		t.Fatalf("LoadMemory: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 memory facts from 2 iterations, got %d: %v", len(facts), facts)
	}
}

// ---------------------------------------------------------------------------
// Codex agent name integration tests
// ---------------------------------------------------------------------------

// When agentName is "Codex", the system prompt and feedback use "Codex" instead of "Claude Code".
func TestIntegration_AutonomousLoop_CodexAgentName(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("CODEX_%d", time.Now().UnixNano())
	callCount := 0
	var capturedMessages [][]orchestrator.Message

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req orchestrator.Request
		json.NewDecoder(r.Body).Decode(&req)
		callCount++
		snapshot := make([]orchestrator.Message, len(req.Messages))
		copy(snapshot, req.Messages)
		capturedMessages = append(capturedMessages, snapshot)

		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)
	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "codex agent test", "Codex", nil, nil)

	// Verify system prompt uses "Codex" and not "Claude Code".
	if len(capturedMessages) < 1 {
		t.Fatal("expected at least 1 API call")
	}
	sysPrompt := capturedMessages[0][0].Content
	if !strings.Contains(sysPrompt, "Codex CLI") {
		t.Fatal("system prompt should reference 'Codex CLI'")
	}
	if strings.Contains(sysPrompt, "Claude Code") {
		t.Fatal("system prompt should not contain 'Claude Code' when agent is Codex")
	}

	// Verify feedback message uses "Codex output:" instead of "Claude Code output:".
	if len(capturedMessages) >= 2 {
		call2 := capturedMessages[1]
		foundCodexFeedback := false
		for _, msg := range call2 {
			if msg.Role == "user" && strings.Contains(msg.Content, "Codex output:") {
				foundCodexFeedback = true
				break
			}
		}
		if !foundCodexFeedback {
			t.Fatal("second API call should contain 'Codex output:' in user feedback")
		}
	}
}

// SSE events include both agent_output and claude_output fields for backward compatibility.
func TestIntegration_AutonomousLoop_SSEAgentOutput(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("SSEAGENT_%d", time.Now().UnixNano())
	callCount := 0

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		switch callCount {
		case 1:
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)

	broker := dashboard.NewSSEBroker()
	ch, unsub := broker.Subscribe()
	defer unsub()

	orchestrator.AutonomousLoop(session, workDir, command, "test-key", "test-model", "agent output test", "Claude Code", broker, nil)

	var iterEndEvents []dashboard.IterationEvent
	for {
		select {
		case msg := <-ch:
			payload := strings.TrimPrefix(strings.TrimSpace(msg), "data: ")
			var evt dashboard.IterationEvent
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				if evt.Type == "iteration_end" {
					iterEndEvents = append(iterEndEvents, evt)
				}
			}
		default:
			goto done
		}
	}
done:

	if len(iterEndEvents) == 0 {
		t.Fatal("no iteration_end events received")
	}

	first := iterEndEvents[0]
	// Both fields should be set for backward compatibility.
	if first.ClaudeOutput == "" {
		t.Fatal("iteration_end claude_output should be set")
	}
	if first.AgentOutput == "" {
		t.Fatal("iteration_end agent_output should be set")
	}
	if first.ClaudeOutput != first.AgentOutput {
		t.Fatalf("claude_output and agent_output should match: %q vs %q", first.ClaudeOutput, first.AgentOutput)
	}
}
