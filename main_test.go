package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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

func TestIsShellEnvAssignment(t *testing.T) {
	if !isShellEnvAssignment("GT_ROLE=refinery") {
		t.Fatal("expected env assignment token")
	}
	if isShellEnvAssignment("--model=sonnet") {
		t.Fatal("flags should not be treated as env assignment")
	}
}

func TestShouldRecoverSession(t *testing.T) {
	if !shouldRecoverSession(errors.New("no server running on /tmp/tmux")) {
		t.Fatal("expected recovery for server loss")
	}
	if shouldRecoverSession(errors.New("permission denied opening file")) {
		t.Fatal("did not expect recovery for unrelated errors")
	}
}

func TestParsePaneStateLine_Dead(t *testing.T) {
	dead, status, current, err := parsePaneStateLine("1\t127\tclaude\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dead || status != 127 || current != "claude" {
		t.Fatalf("unexpected parse: dead=%v status=%d current=%q", dead, status, current)
	}
}

func TestParsePaneStateLine_Alive(t *testing.T) {
	dead, status, current, err := parsePaneStateLine("0\t\tbash\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dead || status != 0 || current != "bash" {
		t.Fatalf("unexpected parse: dead=%v status=%d current=%q", dead, status, current)
	}
}

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

	got, err := waitForPaneUpdateWithCapture("same", 100*time.Millisecond, capture)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "new" {
		t.Fatalf("got %q, want new", got)
	}
}

func TestWaitForPaneUpdateWithCapture_TimeoutNoChanges(t *testing.T) {
	overrideTimers(t)

	capture := func() (string, error) {
		return "same", nil
	}

	got, err := waitForPaneUpdateWithCapture("same", 5*time.Millisecond, capture)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if got != "same" {
		t.Fatalf("got %q, want same", got)
	}
}

func TestWaitForPaneUpdateWithCapture_CaptureError(t *testing.T) {
	capture := func() (string, error) {
		return "", errors.New("boom")
	}

	_, err := waitForPaneUpdateWithCapture("same", 50*time.Millisecond, capture)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "boom" {
		t.Fatalf("unexpected error: %v", err)
	}
}
