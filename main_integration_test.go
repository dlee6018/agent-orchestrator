//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setupIntegration prepares an isolated tmux environment for a single test.
// It creates a dedicated tmux server via a unique socket so tests never touch
// the user's real tmux.  Tests must NOT call t.Parallel — they mutate
// package-level globals (tmuxSocket, pollInterval, etc.).
func setupIntegration(t *testing.T) (session, workDir, command string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available, skipping integration test")
	}

	origPoll := pollInterval
	origStable := stableWindow
	origSettle := startupSettleWindow
	origSocket := tmuxSocket

	socket := fmt.Sprintf("go-orch-inttest-%d", time.Now().UnixNano())
	session = fmt.Sprintf("inttest-%d", time.Now().UnixNano())
	workDir = t.TempDir()
	command = "bash"

	tmuxSocket = socket
	pollInterval = 50 * time.Millisecond
	stableWindow = 500 * time.Millisecond
	startupSettleWindow = 300 * time.Millisecond

	t.Cleanup(func() {
		exec.Command("tmux", "-L", socket, "kill-server").Run()
		tmuxSocket = origSocket
		pollInterval = origPoll
		stableWindow = origStable
		startupSettleWindow = origSettle
	})

	return session, workDir, command
}

// createTestSession is a test helper that calls ensureClaudeSession and lets
// bash settle before returning.
func createTestSession(t *testing.T, session, workDir, command string) {
	t.Helper()
	if err := ensureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("ensureClaudeSession: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// Returns false for a session that was never created.
func TestIntegration_TmuxHasSession_NonExistent(t *testing.T) {
	session, _, _ := setupIntegration(t)

	ok, err := tmuxHasSession(session)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("session should not exist before creation")
	}
}

// A real tmux session is created from scratch when none exists.
func TestIntegration_EnsureClaudeSession_CreatesSession(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	if err := ensureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("ensureClaudeSession: %v", err)
	}

	ok, err := tmuxHasSession(session)
	if err != nil {
		t.Fatalf("tmuxHasSession: %v", err)
	}
	if !ok {
		t.Fatal("session should exist after ensureClaudeSession")
	}
}

// Calling ensureClaudeSession twice is safe — the session survives the second call.
func TestIntegration_EnsureClaudeSession_Idempotent(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	if err := ensureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ensureClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("second call (idempotent): %v", err)
	}

	ok, err := tmuxHasSession(session)
	if err != nil {
		t.Fatalf("tmuxHasSession: %v", err)
	}
	if !ok {
		t.Fatal("session should still exist after duplicate ensure")
	}
}

// Capturing a live pane returns non-empty output.
func TestIntegration_CapturePane(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane: %v", err)
	}
	if len(pane) == 0 {
		t.Fatal("expected non-empty pane capture")
	}
}

// A running bash session reports dead=false and "bash" as the current command.
func TestIntegration_TmuxPaneState_Alive(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	dead, _, currentCmd, err := tmuxPaneState(session)
	if err != nil {
		t.Fatalf("tmuxPaneState: %v", err)
	}
	if dead {
		t.Fatal("pane should not be dead while bash is running")
	}
	if !strings.Contains(currentCmd, "bash") {
		t.Fatalf("expected bash as current command, got %q", currentCmd)
	}
}

// Sending an echo command via sendMessage makes the marker appear in the captured pane.
func TestIntegration_SendMessage_AppearsInPane(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	marker := fmt.Sprintf("SENDTEST_%d", time.Now().UnixNano())
	if err := sendMessage(session, fmt.Sprintf("echo %s", marker)); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	time.Sleep(1 * time.Second)

	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not found in pane:\n%s", marker, pane)
	}
}

// Polling detects new pane content after sending a command and returns once stable.
func TestIntegration_WaitForPaneUpdate(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	initial, err := capturePane(session)
	if err != nil {
		t.Fatalf("initial capture: %v", err)
	}

	marker := fmt.Sprintf("PANEUPD_%d", time.Now().UnixNano())
	if err := sendMessage(session, fmt.Sprintf("echo %s", marker)); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	pane, err := waitForPaneUpdate(session, initial, 10*time.Second)
	if err != nil {
		t.Fatalf("waitForPaneUpdate: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in updated pane:\n%s", marker, pane)
	}
}

// After a restart, the session exists and can execute commands.
func TestIntegration_RestartSession(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	if err := restartClaudeSession(session, workDir, command); err != nil {
		t.Fatalf("restartClaudeSession: %v", err)
	}

	ok, err := tmuxHasSession(session)
	if err != nil {
		t.Fatalf("tmuxHasSession after restart: %v", err)
	}
	if !ok {
		t.Fatal("session should exist after restart")
	}

	// Verify the restarted session is functional.
	time.Sleep(300 * time.Millisecond)
	marker := fmt.Sprintf("RESTART_%d", time.Now().UnixNano())
	if err := sendMessage(session, fmt.Sprintf("echo %s", marker)); err != nil {
		t.Fatalf("sendMessage after restart: %v", err)
	}
	time.Sleep(1 * time.Second)

	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane after restart: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in pane after restart:\n%s", marker, pane)
	}
}

// Normal send-and-capture succeeds without needing recovery.
func TestIntegration_SendAndCaptureWithRecovery_HappyPath(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	initial, err := capturePane(session)
	if err != nil {
		t.Fatalf("initial capture: %v", err)
	}

	marker := fmt.Sprintf("HAPPY_%d", time.Now().UnixNano())
	pane, err := sendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), initial)
	if err != nil {
		t.Fatalf("sendAndCaptureWithRecovery: %v", err)
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
	if err := runTmux("kill-session", "-t", session); err != nil {
		t.Fatalf("kill-session: %v", err)
	}

	ok, err := tmuxHasSession(session)
	if err != nil {
		t.Fatalf("tmuxHasSession after kill: %v", err)
	}
	if ok {
		t.Fatal("session should be gone after kill")
	}

	// sendAndCaptureWithRecovery should recreate the session and succeed.
	marker := fmt.Sprintf("RECOVER_%d", time.Now().UnixNano())
	pane, err := sendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), "")
	if err != nil {
		t.Fatalf("sendAndCaptureWithRecovery after kill: %v", err)
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
	exec.Command("tmux", "-L", tmuxSocket, "kill-server").Run()
	time.Sleep(200 * time.Millisecond)

	marker := fmt.Sprintf("SRVRECOV_%d", time.Now().UnixNano())
	pane, err := sendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), "")
	if err != nil {
		t.Fatalf("sendAndCaptureWithRecovery after server kill: %v", err)
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
		pane, err := sendAndCaptureWithRecovery(session, workDir, command, fmt.Sprintf("echo %s", marker), lastPane)
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if !strings.Contains(pane, marker) {
			t.Fatalf("message %d: marker %q not in pane:\n%s", i, marker, pane)
		}
		lastPane = pane
	}
}

// cleanupSession removes the tmux session so it no longer exists.
func TestIntegration_CleanupSession(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	ok, err := tmuxHasSession(session)
	if err != nil {
		t.Fatalf("tmuxHasSession before cleanup: %v", err)
	}
	if !ok {
		t.Fatal("session should exist before cleanup")
	}

	cleanupSession(session)

	ok, err = tmuxHasSession(session)
	if err != nil {
		t.Fatalf("tmuxHasSession after cleanup: %v", err)
	}
	if ok {
		t.Fatal("session should not exist after cleanupSession")
	}
}

// ---------------------------------------------------------------------------
// Autonomous mode integration tests
// ---------------------------------------------------------------------------

// setupAutonomous configures openRouterEndpoint and maxIterations for autonomous
// loop tests, restoring them on cleanup.
func setupAutonomous(t *testing.T, endpoint string, max int) {
	t.Helper()
	oldEndpoint := openRouterEndpoint
	oldMax := maxIterations
	openRouterEndpoint = endpoint
	maxIterations = max
	t.Cleanup(func() {
		openRouterEndpoint = oldEndpoint
		maxIterations = oldMax
	})
}

// mockOpenRouter creates an httptest server that calls handler for each request.
// Returns the server (caller should defer srv.Close()).
func mockOpenRouter(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// respondJSON writes an openRouterResponse as JSON.
func respondJSON(w http.ResponseWriter, reply string, callID int) {
	resp := openRouterResponse{
		ID: fmt.Sprintf("call-%d", callID),
		Choices: []openRouterChoice{
			{Message: openRouterMessage{Role: "assistant", Content: reply}, FinishReason: "stop"},
		},
		Usage: openRouterUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

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

	autonomousLoop(session, workDir, command, "test-key", "test-model", "echo a marker", nil, nil)

	if callCount < 2 {
		t.Fatalf("expected at least 2 API calls, got %d", callCount)
	}

	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane: %v", err)
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

	autonomousLoop(session, workDir, command, "test-key", "test-model", "never-ending task", nil, nil)

	if callCount != 3 {
		t.Fatalf("expected exactly 3 API calls (maxIterations=3), got %d", callCount)
	}
}

// The autonomous loop passes Claude Code output back to the LLM in conversation history.
func TestIntegration_AutonomousLoop_FeedbackLoop(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("FEEDBACK_%d", time.Now().UnixNano())
	callCount := 0
	var secondCallMessages []openRouterMessage

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req openRouterRequest
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
	autonomousLoop(session, workDir, command, "test-key", "test-model", "echo test", nil, nil)

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
	var allCapturedMessages [][]openRouterMessage

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req openRouterRequest
		json.NewDecoder(r.Body).Decode(&req)
		callCount++
		// Snapshot the messages array for each call.
		snapshot := make([]openRouterMessage, len(req.Messages))
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
	autonomousLoop(session, workDir, command, "test-key", "test-model", "run two commands", nil, nil)

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
	if !strings.Contains(call1[0].Content, taskCompleteMarker) {
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
		t.Fatal("call 2: user feedback message missing 'Claude Code output:' prefix")
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
	autonomousLoop(session, workDir, command, "test-key", "test-model", "embedded completion test", nil, nil)

	// Should have completed after 2 iterations (not run to maxIterations).
	if callCount != 2 {
		t.Fatalf("expected 2 API calls (TASK_COMPLETE embedded in call 2), got %d", callCount)
	}

	// Verify the first command actually ran.
	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane: %v", err)
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
		errResp := openRouterError{}
		errResp.Error.Message = "server on fire"
		errResp.Error.Code = 500
		json.NewEncoder(w).Encode(errResp)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)

	start := time.Now()
	autonomousLoop(session, workDir, command, "test-key", "test-model", "doomed task", nil, nil)
	elapsed := time.Since(start)

	calls := int(atomic.LoadInt32(&apiCalls))
	if calls != 3 {
		t.Fatalf("expected exactly 3 API calls before abort, got %d", calls)
	}
	// Should have waited ~10s (two 5s sleeps between the 3 attempts).
	// Allow generous bounds since timing is approximate.
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
			errResp := openRouterError{}
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
	autonomousLoop(session, workDir, command, "test-key", "test-model", "transient error task", nil, nil)

	calls := int(atomic.LoadInt32(&apiCalls))
	if calls != 3 {
		t.Fatalf("expected 3 API calls (1 error + 1 command + 1 complete), got %d", calls)
	}

	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane: %v", err)
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
	var capturedMessages [][]openRouterMessage

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req openRouterRequest
		json.NewDecoder(r.Body).Decode(&req)
		callCount++
		snapshot := make([]openRouterMessage, len(req.Messages))
		copy(snapshot, req.Messages)
		capturedMessages = append(capturedMessages, snapshot)

		switch callCount {
		case 1:
			// Step 1: Create a file.
			respondJSON(w, fmt.Sprintf("echo '%s' > testfile.txt", marker), callCount)
		case 2:
			// Step 2: Read the file back.
			respondJSON(w, "cat testfile.txt", callCount)
		case 3:
			// Step 3: Verify the output contains the marker — the LLM "sees" it
			// in the conversation history and signals done.
			respondJSON(w, "TASK_COMPLETE", callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)
	autonomousLoop(session, workDir, command, "test-key", "test-model", "create and verify file", nil, nil)

	if callCount != 3 {
		t.Fatalf("expected 3 API calls for multi-step task, got %d", callCount)
	}

	// The third call's messages should contain the marker from the cat output.
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

// Tmux session killed mid-loop: the error is fed back to the LLM, which can then
// adapt and continue (the session auto-recovers via sendAndCaptureWithRecovery).
func TestIntegration_AutonomousLoop_TmuxRecoveryMidLoop(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	marker := fmt.Sprintf("TMXRECOV_%d", time.Now().UnixNano())
	callCount := 0
	var capturedMessages [][]openRouterMessage

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		var req openRouterRequest
		json.NewDecoder(r.Body).Decode(&req)
		callCount++
		snapshot := make([]openRouterMessage, len(req.Messages))
		copy(snapshot, req.Messages)
		capturedMessages = append(capturedMessages, snapshot)

		switch callCount {
		case 1:
			// First command: echo something so we have tmux activity.
			respondJSON(w, "echo before_kill", callCount)
		case 2:
			// This call happens after the session was killed and recovered.
			// The previous iteration should have gotten an error or succeeded
			// after recovery. Send a command to verify the session is alive.
			respondJSON(w, fmt.Sprintf("echo %s", marker), callCount)
		default:
			respondJSON(w, "TASK_COMPLETE", callCount)
		}
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 10)
	createTestSession(t, session, workDir, command)

	// Kill the session after a brief delay (during iteration 1's sendAndCapture).
	// sendAndCaptureWithRecovery should handle this internally.
	go func() {
		time.Sleep(1 * time.Second)
		_ = runTmux("kill-session", "-t", session)
	}()

	autonomousLoop(session, workDir, command, "test-key", "test-model", "survive tmux kill", nil, nil)

	if callCount < 2 {
		t.Fatalf("expected at least 2 API calls, got %d", callCount)
	}

	// Verify the session recovered and the second marker command ran.
	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane after recovery: %v", err)
	}
	if !strings.Contains(pane, marker) {
		t.Fatalf("marker %q not in pane after tmux recovery:\n%s", marker, pane)
	}
}

// cleanPaneOutput strips ANSI sequences from real tmux pane captures.
func TestIntegration_CleanPaneOutput_RealPane(t *testing.T) {
	session, workDir, command := setupIntegration(t)
	createTestSession(t, session, workDir, command)

	marker := fmt.Sprintf("CLEAN_%d", time.Now().UnixNano())

	// Use printf with ANSI color codes to generate colored output.
	colorCmd := fmt.Sprintf(`printf '\033[31m%s\033[0m\n'`, marker)
	if err := sendMessage(session, colorCmd); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}
	time.Sleep(1 * time.Second)

	pane, err := capturePane(session)
	if err != nil {
		t.Fatalf("capturePane: %v", err)
	}

	cleaned := cleanPaneOutput(pane)
	if !strings.Contains(cleaned, marker) {
		t.Fatalf("marker %q not in cleaned output:\n%s", marker, cleaned)
	}
	// The raw ANSI escape should be gone.
	if strings.Contains(cleaned, "\x1b[") {
		t.Fatalf("cleaned output still contains ANSI escapes:\n%s", cleaned)
	}
}

// API key and model are passed correctly in the HTTP request.
func TestIntegration_AutonomousLoop_APIKeyAndModel(t *testing.T) {
	session, workDir, command := setupIntegration(t)

	var receivedAuth string
	var receivedModel string

	srv := mockOpenRouter(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		var req openRouterRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedModel = req.Model
		respondJSON(w, "TASK_COMPLETE", 1)
	})
	defer srv.Close()

	setupAutonomous(t, srv.URL, 5)
	createTestSession(t, session, workDir, command)
	autonomousLoop(session, workDir, command, "sk-test-key-123", "anthropic/claude-sonnet-4", "api key test", nil, nil)

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

	initial, err := capturePane(session)
	if err != nil {
		t.Fatalf("initial capture: %v", err)
	}

	autonomousLoop(session, workDir, command, "test-key", "test-model", "instant done", nil, nil)

	if callCount != 1 {
		t.Fatalf("expected exactly 1 API call, got %d", callCount)
	}

	// Pane should be unchanged — no commands were sent.
	after, err := capturePane(session)
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

// The autonomous loop emits the expected SSE events (task_info, iteration_start,
// iteration_end, complete) when given a broker.
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

	broker := newSSEBroker()
	ch, unsub := broker.subscribe()
	defer unsub()

	autonomousLoop(session, workDir, command, "test-key", "test-model", "emit SSE events", broker, nil)

	// Drain all events from the channel.
	var events []iterationEvent
	for {
		select {
		case msg := <-ch:
			// Parse SSE data line: "data: {...}\n\n"
			payload := strings.TrimPrefix(strings.TrimSpace(msg), "data: ")
			var evt iterationEvent
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				events = append(events, evt)
			}
		default:
			goto collected
		}
	}
collected:

	// Verify we got the expected event types in order.
	if len(events) < 4 {
		t.Fatalf("expected at least 4 SSE events, got %d", len(events))
	}

	// First event should be task_info.
	if events[0].Type != "task_info" {
		t.Fatalf("event 0: expected task_info, got %q", events[0].Type)
	}
	if events[0].Task != "emit SSE events" {
		t.Fatalf("task_info: expected task %q, got %q", "emit SSE events", events[0].Task)
	}
	if events[0].Model != "test-model" {
		t.Fatalf("task_info: expected model %q, got %q", "test-model", events[0].Model)
	}

	// Check that we have iteration_start and iteration_end events.
	typeSet := make(map[string]bool)
	for _, evt := range events {
		typeSet[evt.Type] = true
	}
	for _, want := range []string{"task_info", "iteration_start", "iteration_end", "complete"} {
		if !typeSet[want] {
			t.Fatalf("missing event type %q; got types: %v", want, typeSet)
		}
	}

	// The last event should be "complete" with no error (successful task).
	last := events[len(events)-1]
	if last.Type != "complete" {
		t.Fatalf("last event: expected complete, got %q", last.Type)
	}
	if last.Error != "" {
		t.Fatalf("last event: unexpected error: %s", last.Error)
	}

	// Verify iteration_end events have token data.
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

	broker := newSSEBroker()
	ch, unsub := broker.subscribe()
	defer unsub()

	autonomousLoop(session, workDir, command, "test-key", "test-model", "content test", broker, nil)

	// Drain events and find the first iteration_end with Claude output.
	var iterEndEvents []iterationEvent
	for {
		select {
		case msg := <-ch:
			payload := strings.TrimPrefix(strings.TrimSpace(msg), "data: ")
			var evt iterationEvent
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

	// The first iteration_end should contain the echo command as orchestrator message.
	first := iterEndEvents[0]
	if !strings.Contains(first.Orchestrator, marker) {
		t.Fatalf("iteration_end orchestrator message missing marker %q: %q", marker, first.Orchestrator)
	}
	// Claude Code should have echoed the marker back.
	if !strings.Contains(first.ClaudeOutput, marker) {
		t.Fatalf("iteration_end claude_output missing marker %q: %q", marker, first.ClaudeOutput)
	}
	// Duration should be positive.
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

	broker := newSSEBroker()
	ch, unsub := broker.subscribe()
	defer unsub()

	autonomousLoop(session, workDir, command, "test-key", "test-model", "max iter test", broker, nil)

	// Drain and find the complete event.
	var completeEvent *iterationEvent
	for {
		select {
		case msg := <-ch:
			payload := strings.TrimPrefix(strings.TrimSpace(msg), "data: ")
			var evt iterationEvent
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
