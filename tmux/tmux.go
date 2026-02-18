package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	runtimeReadyTTL = 10 * time.Second
)

// Socket is the tmux socket name for session isolation.
var Socket string

// PollInterval is how often to poll for pane updates.
var PollInterval = 500 * time.Millisecond // 0.5s

// StableWindow is how long pane content must be unchanged to be considered stable.
var StableWindow = 2 * time.Second // 2s

// StartupSettleWindow is how long to wait after startup to confirm session is alive.
var StartupSettleWindow = 1500 * time.Millisecond // 1.5s

// KeystrokeSleep is the pause between sending text and pressing Enter.
var KeystrokeSleep = 500 * time.Millisecond // pause between text and Enter

// MaxSendRetries is the number of attempts for send-and-capture (1 initial + retries).
var MaxSendRetries = 2 // 1 initial attempt + 1 retry

// ansiPattern matches ANSI escape sequences (CSI sequences and OSC sequences).
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\|\x1b\][^\x07]*\x07`)

// blankRunPattern matches 3+ consecutive blank lines.
var blankRunPattern = regexp.MustCompile(`(\n\s*){3,}`)

// EnsureClaudeSession creates a new tmux session or validates/restarts an existing one.
func EnsureClaudeSession(session, workDir, command string) error {
	ok, err := tmuxHasSession(session)
	if err != nil {
		return err
	}
	if !ok {
		if err := createSession(session, workDir, command); err != nil {
			return err
		}
	} else {
		dead, status, currentCmd, err := tmuxPaneState(session)
		if err != nil {
			return err
		}
		if dead {
			if err := restartClaudeSession(session, workDir, command); err != nil {
				return fmt.Errorf("EnsureClaudeSession: recover dead pane (status %d, cmd %q): %w", status, currentCmd, err)
			}
		}
	}

	if err := WaitForRuntimeReady(session, command, runtimeReadyTTL); err != nil {
		return err
	}
	return nil
}

// setTmuxServerOptions configures global tmux options.  Must be called
// after at least one session exists so the server is guaranteed running.
func setTmuxServerOptions() error {
	if err := RunTmux("set-option", "-g", "exit-empty", "off"); err != nil {
		return fmt.Errorf("setTmuxServerOptions: set exit-empty: %w", err)
	}
	return nil
}

// configureSession sets remain-on-exit so dead panes stay around for diagnostics.
func configureSession(session string) error {
	// Keep the pane around if Claude exits so we can capture diagnostics.
	if err := RunTmux("set-window-option", "-t", session, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("configureSession: set remain-on-exit: %w", err)
	}
	return nil
}

// SendAndCaptureWithRecovery sends a message and captures the response, retrying once on recoverable failures.
func SendAndCaptureWithRecovery(session, workDir, command, message, lastPane string) (string, error) {
	var lastErr error

	for attempt := 1; attempt <= MaxSendRetries; attempt++ {
		if err := EnsureClaudeSession(session, workDir, command); err != nil {
			lastErr = fmt.Errorf("SendAndCaptureWithRecovery: ensure session: %w", err)
			if ShouldRecoverSession(err) && attempt == 1 {
				if restartErr := restartClaudeSession(session, workDir, command); restartErr != nil {
					lastErr = fmt.Errorf("SendAndCaptureWithRecovery: ensure session retry: %v: %w", lastErr, restartErr)
					continue
				}
				continue
			}
			return "", lastErr
		}

		if err := SendMessage(session, message); err != nil {
			lastErr = fmt.Errorf("SendAndCaptureWithRecovery: send message: %w", err)
			if ShouldRecoverSession(err) && attempt == 1 {
				if restartErr := restartClaudeSession(session, workDir, command); restartErr != nil {
					lastErr = fmt.Errorf("SendAndCaptureWithRecovery: send message retry: %v: %w", lastErr, restartErr)
					continue
				}
				continue
			}
			return "", lastErr
		}

		pane, err := WaitForPaneUpdate(session, lastPane, 90*time.Second)
		if err != nil {
			lastErr = fmt.Errorf("SendAndCaptureWithRecovery: capture pane: %w", err)
			if ShouldRecoverSession(err) && attempt == 1 {
				if restartErr := restartClaudeSession(session, workDir, command); restartErr != nil {
					lastErr = fmt.Errorf("SendAndCaptureWithRecovery: capture pane retry: %v: %w", lastErr, restartErr)
					continue
				}
				continue
			}
			return pane, lastErr
		}

		return pane, nil
	}

	if lastErr == nil {
		lastErr = errors.New("unknown retry failure")
	}
	return "", lastErr
}

// SendMessage sends text to the tmux pane as literal keystrokes followed by Enter.
func SendMessage(session, message string) error {
	if err := RunTmux("send-keys", "-t", session, "-l", message); err != nil {
		return fmt.Errorf("SendMessage: send-keys literal: %w", err)
	}
	time.Sleep(KeystrokeSleep)
	if err := RunTmux("send-keys", "-t", session, "C-m"); err != nil {
		return fmt.Errorf("SendMessage: send-keys enter: %w", err)
	}
	return nil
}

// WaitForPaneUpdate polls the tmux pane until its content changes and stabilizes.
func WaitForPaneUpdate(session, previous string, timeout time.Duration) (string, error) {
	return WaitForPaneUpdateWithCapture(previous, timeout, func() (string, error) {
		return CapturePane(session)
	}, func() (bool, error) {
		dead, _, _, err := tmuxPaneState(session)
		if err != nil {
			return false, err
		}
		return !dead, nil
	})
}

// WaitForPaneUpdateWithCapture is the testable core of WaitForPaneUpdate using injectable capture and checkAlive funcs.
func WaitForPaneUpdateWithCapture(previous string, timeout time.Duration, capture func() (string, error), checkAlive func() (bool, error)) (string, error) {
	deadline := time.Now().Add(timeout)
	last := previous
	stableSince := time.Now()

	for time.Now().Before(deadline) {
		pane, err := capture()
		if err != nil {
			return "", err
		}
		if pane != last {
			last = pane
			stableSince = time.Now()
		} else if pane != previous && time.Since(stableSince) >= StableWindow {
			return pane, nil
		}

		time.Sleep(PollInterval)
	}

	if last == previous {
		alive, aliveErr := checkAlive()
		if aliveErr != nil {
			return last, fmt.Errorf("WaitForPaneUpdateWithCapture: liveness check failed: %w", aliveErr)
		}
		if !alive {
			return last, fmt.Errorf("WaitForPaneUpdateWithCapture: claude code process is dead, no pane changes within %s", timeout)
		}
		return last, fmt.Errorf("WaitForPaneUpdateWithCapture: claude code is still working, no pane changes within %s", timeout)
	}
	return last, fmt.Errorf("WaitForPaneUpdateWithCapture: timeout (%s) reached, content changed but did not stabilize", timeout)
}

// WaitForRuntimeReady blocks until the tmux session is alive and the startup command hasn't crashed.
func WaitForRuntimeReady(session, startupCommand string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		ok, err := tmuxHasSession(session)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("WaitForRuntimeReady: session %q exited during startup; command may have failed: %q", session, startupCommand)
		}

		dead, deadStatus, currentCmd, err := tmuxPaneState(session)
		if err != nil {
			return err
		}
		if dead {
			paneText, _ := CapturePane(session)
			return fmt.Errorf("WaitForRuntimeReady: process exited (status %d, cmd %q) for %q; pane output:\n%s", deadStatus, currentCmd, startupCommand, paneText)
		}

		_, err = CapturePane(session)
		if err == nil {
			if err := requireSessionAliveFor(session, StartupSettleWindow); err != nil {
				lastErr = err
				time.Sleep(250 * time.Millisecond)
				continue
			}
			return nil
		}
		lastErr = err

		time.Sleep(250 * time.Millisecond)
	}

	if lastErr != nil {
		return fmt.Errorf("WaitForRuntimeReady: session %q not ready within %s: %w", session, timeout, lastErr)
	}
	return fmt.Errorf("WaitForRuntimeReady: session %q not ready within %s", session, timeout)
}

// requireSessionAliveFor verifies the session stays alive for the given duration (guards against fast crashes).
func requireSessionAliveFor(session string, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		ok, err := tmuxHasSession(session)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("requireSessionAliveFor: session %q exited shortly after startup", session)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// tmuxHasSession returns true if the named tmux session exists.
func tmuxHasSession(session string) (bool, error) {
	err := RunTmux("has-session", "-t", session)
	if err == nil {
		return true, nil
	}
	if isTmuxNotFoundError(err) {
		return false, nil
	}
	return false, err
}

// isTmuxNotFoundError returns true for any tmux error that means
// "the session (or server) does not exist".
func isTmuxNotFoundError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "no current target") ||
		strings.Contains(msg, "error connecting to")
}

// tmuxPaneState returns whether the pane is dead, its exit status, and current command.
func tmuxPaneState(session string) (bool, int, string, error) {
	cmd := exec.Command("tmux", TmuxArgs("list-panes", "-t", session, "-F", "#{pane_dead}\t#{pane_dead_status}\t#{pane_current_command}")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, 0, "", fmt.Errorf("tmuxPaneState: list-panes -t %s: %w (%s)", session, err, strings.TrimSpace(string(out)))
	}
	return ParsePaneStateLine(string(out))
}

// ParsePaneStateLine parses a tab-separated "dead\tstatus\tcommand" line from list-panes.
func ParsePaneStateLine(raw string) (bool, int, string, error) {
	line := strings.TrimSpace(raw)
	if line == "" {
		return false, 0, "", errors.New("ParsePaneStateLine: empty output from list-panes")
	}
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) < 3 {
		return false, 0, "", fmt.Errorf("ParsePaneStateLine: unexpected format: %q", line)
	}
	dead := strings.TrimSpace(parts[0]) == "1"
	status := 0
	if s := strings.TrimSpace(parts[1]); s != "" {
		var err error
		status, err = strconv.Atoi(s)
		if err != nil {
			return false, 0, "", fmt.Errorf("ParsePaneStateLine: parse status %q: %w", s, err)
		}
	}
	return dead, status, strings.TrimSpace(parts[2]), nil
}

// ResolveStartupCommand validates the command string, resolves the binary
// to an absolute path via LookPath, and returns the normalised command.
// NOTE: The command is split on whitespace (strings.Fields), so quoted
// arguments containing spaces (e.g. --prompt "hello world") are not
// supported. Wrap such commands in a shell script if needed.
func ResolveStartupCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("ResolveStartupCommand: command is empty")
	}
	fields := strings.Fields(command)
	idx := -1
	for i, f := range fields {
		if IsShellEnvAssignment(f) {
			continue
		}
		idx = i
		break
	}
	if idx < 0 {
		return "", errors.New("ResolveStartupCommand: missing executable")
	}
	binPath, err := exec.LookPath(fields[idx])
	if err != nil {
		return "", fmt.Errorf("ResolveStartupCommand: binary %q not found in PATH", fields[idx])
	}
	fields[idx] = binPath
	return strings.Join(fields, " "), nil
}

// IsShellEnvAssignment returns true if the token looks like KEY=VALUE (e.g. "FOO=bar").
func IsShellEnvAssignment(token string) bool {
	eq := strings.IndexByte(token, '=')
	if eq <= 0 {
		return false
	}
	name := token[:eq]
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

// ShouldRecoverSession checks if an error indicates the tmux session/server is gone and worth restarting.
func ShouldRecoverSession(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "session not ready") ||
		strings.Contains(msg, "exited during startup") ||
		strings.Contains(msg, "exited shortly after startup") ||
		strings.Contains(msg, "startup process exited")
}

// createSession starts a new tmux session and applies server/session options.
// new-session implicitly starts the tmux server if needed, avoiding the race
// where start-server exits before we can set options.
func createSession(session, workDir, command string) error {
	if err := RunTmux("new-session", "-d", "-s", session, "-c", workDir, command); err != nil {
		return fmt.Errorf("createSession: new-session: %w", err)
	}
	if err := setTmuxServerOptions(); err != nil {
		return err
	}
	return configureSession(session)
}

// restartClaudeSession kills the existing session and creates a fresh one.
func restartClaudeSession(session, workDir, command string) error {
	if err := RunTmux("kill-session", "-t", session); err != nil {
		if !isTmuxNotFoundError(err) {
			return fmt.Errorf("restartClaudeSession: kill session: %w", err)
		}
	}
	if err := createSession(session, workDir, command); err != nil {
		return err
	}
	return WaitForRuntimeReady(session, command, runtimeReadyTTL)
}

// CapturePane returns the full visible text of the tmux pane.
func CapturePane(session string) (string, error) {
	cmd := exec.Command("tmux", TmuxArgs("capture-pane", "-p", "-t", session, "-S", "-")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("CapturePane: capture-pane: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// RunTmux executes a tmux command with the configured socket and returns any error.
func RunTmux(args ...string) error {
	cmd := exec.Command("tmux", TmuxArgs(args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// TmuxArgs prepends the -L socket flag to isolate from the user's default tmux server.
func TmuxArgs(args ...string) []string {
	if strings.TrimSpace(Socket) == "" {
		return args
	}
	return append([]string{"-L", Socket}, args...)
}

// CleanupSession kills the tmux session, ignoring errors.
func CleanupSession(session string) {
	_ = RunTmux("kill-session", "-t", session)
}

// CleanPaneOutput strips ANSI escape sequences, collapses excessive blank
// lines, and trims trailing whitespace.
func CleanPaneOutput(raw string) string {
	s := ansiPattern.ReplaceAllString(raw, "")
	// Trim trailing whitespace per line.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t\r")
	}
	s = strings.Join(lines, "\n")
	// Collapse runs of 3+ blank lines to 2.
	s = blankRunPattern.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// TruncateForLog truncates s to maxLen characters, appending "..." if truncated.
func TruncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
