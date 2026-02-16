//go:build integration

package main

import (
	"fmt"
	"os/exec"
	"strings"
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
