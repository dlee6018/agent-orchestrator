package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultSession  = "gt-claude-loop"
	defaultSocket   = "gt-claude-loop"
	defaultCommand  = "claude --dangerously-skip-permissions --setting-sources user"
	runtimeReadyTTL = 10 * time.Second
)

var tmuxSocket string
var pollInterval = 500 * time.Millisecond
var stableWindow = 2 * time.Second
var startupSettleWindow = 1500 * time.Millisecond

func main() {
	session := envOrDefault("CLAUDE_TMUX_SESSION", defaultSession)
	tmuxSocket = envOrDefault("CLAUDE_TMUX_SOCKET", defaultSocket)
	if err := validateSessionName(session); err != nil {
		fmt.Fprintf(os.Stderr, "invalid session name: %v\n", err)
		os.Exit(1)
	}
	if err := validateSessionName(tmuxSocket); err != nil {
		fmt.Fprintf(os.Stderr, "invalid socket name: %v\n", err)
		os.Exit(1)
	}
	command, err := resolveStartupCommand(envOrDefault("CLAUDE_CMD", defaultCommand))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid startup command: %v\n", err)
		os.Exit(1)
	}
	workDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve working directory: %v\n", err)
		os.Exit(1)
	}

	if err := ensureClaudeSession(session, workDir, command); err != nil {
		fmt.Fprintf(os.Stderr, "failed to prepare session: %v\n", err)
		os.Exit(1)
	}

	terminateOnQuit := envBool("TERMINATE_WHEN_QUIT", false)

	fmt.Printf("Session %q is ready. Type messages and press Enter. Use /quit to exit.\n", session)

	if terminateOnQuit {
		cleanup := func() { cleanupSession(session) }

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintln(os.Stderr, "\nsignal received, cleaning up tmux session...")
			cleanup()
			os.Exit(0)
		}()

		chatLoop(session, workDir, command)
		cleanup()
	} else {
		chatLoop(session, workDir, command)
	}
}

func chatLoop(session, workDir, command string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1 MB max input
	lastPane := ""

	for {
		fmt.Print("you> ")
		if !scanner.Scan() {
			fmt.Println("\ninput closed")
			return
		}

		message := strings.TrimSpace(scanner.Text())
		if message == "" {
			continue
		}
		if message == "/quit" {
			return
		}

		pane, err := sendAndCaptureWithRecovery(session, workDir, command, message, lastPane)
		if err != nil {
			fmt.Fprintf(os.Stderr, "message failed: %v\n", err)
			continue
		}

		fmt.Println("\n----- full tmux pane output -----")
		fmt.Println(pane)
		fmt.Println("----- end output -----")
		lastPane = pane
	}
}

func ensureClaudeSession(session, workDir, command string) error {
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
				return fmt.Errorf("ensureClaudeSession: recover dead pane (status %d, cmd %q): %w", status, currentCmd, err)
			}
		}
	}

	if err := waitForRuntimeReady(session, command, runtimeReadyTTL); err != nil {
		return err
	}
	return nil
}

// setTmuxServerOptions configures global tmux options.  Must be called
// after at least one session exists so the server is guaranteed running.
func setTmuxServerOptions() error {
	if err := runTmux("set-option", "-g", "exit-empty", "off"); err != nil {
		return fmt.Errorf("setTmuxServerOptions: set exit-empty: %w", err)
	}
	return nil
}

func configureSession(session string) error {
	// Keep the pane around if Claude exits so we can capture diagnostics.
	if err := runTmux("set-window-option", "-t", session, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("configureSession: set remain-on-exit: %w", err)
	}
	return nil
}

func sendAndCaptureWithRecovery(session, workDir, command, message, lastPane string) (string, error) {
	var lastErr error

	for attempt := 1; attempt <= 2; attempt++ {
		if err := ensureClaudeSession(session, workDir, command); err != nil {
			lastErr = fmt.Errorf("sendAndCaptureWithRecovery: ensure session: %w", err)
			if shouldRecoverSession(err) && attempt == 1 {
				if restartErr := restartClaudeSession(session, workDir, command); restartErr != nil {
					lastErr = fmt.Errorf("sendAndCaptureWithRecovery: ensure session retry: %v: %w", lastErr, restartErr)
					continue
				}
				continue
			}
			return "", lastErr
		}

		if err := sendMessage(session, message); err != nil {
			lastErr = fmt.Errorf("sendAndCaptureWithRecovery: send message: %w", err)
			if shouldRecoverSession(err) && attempt == 1 {
				if restartErr := restartClaudeSession(session, workDir, command); restartErr != nil {
					lastErr = fmt.Errorf("sendAndCaptureWithRecovery: send message retry: %v: %w", lastErr, restartErr)
					continue
				}
				continue
			}
			return "", lastErr
		}

		pane, err := waitForPaneUpdate(session, lastPane, 90*time.Second)
		if err != nil {
			lastErr = fmt.Errorf("sendAndCaptureWithRecovery: capture pane: %w", err)
			if shouldRecoverSession(err) && attempt == 1 {
				if restartErr := restartClaudeSession(session, workDir, command); restartErr != nil {
					lastErr = fmt.Errorf("sendAndCaptureWithRecovery: capture pane retry: %v: %w", lastErr, restartErr)
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

func sendMessage(session, message string) error {
	if err := runTmux("send-keys", "-t", session, "-l", message); err != nil {
		return fmt.Errorf("sendMessage: send-keys literal: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := runTmux("send-keys", "-t", session, "C-m"); err != nil {
		return fmt.Errorf("sendMessage: send-keys enter: %w", err)
	}
	return nil
}

func waitForPaneUpdate(session, previous string, timeout time.Duration) (string, error) {
	return waitForPaneUpdateWithCapture(previous, timeout, func() (string, error) {
		return capturePane(session)
	})
}

func waitForPaneUpdateWithCapture(previous string, timeout time.Duration, capture func() (string, error)) (string, error) {
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
		} else if pane != previous && time.Since(stableSince) >= stableWindow {
			return pane, nil
		}

		time.Sleep(pollInterval)
	}

	if last == previous {
		return last, errors.New("waitForPaneUpdateWithCapture: timeout waiting for pane changes")
	}
	return last, errors.New("waitForPaneUpdateWithCapture: timeout reached, returning latest pane")
}

func waitForRuntimeReady(session, startupCommand string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		ok, err := tmuxHasSession(session)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("waitForRuntimeReady: session %q exited during startup; command may have failed: %q", session, startupCommand)
		}

		dead, deadStatus, currentCmd, err := tmuxPaneState(session)
		if err != nil {
			return err
		}
		if dead {
			paneText, _ := capturePane(session)
			return fmt.Errorf("waitForRuntimeReady: process exited (status %d, cmd %q) for %q; pane output:\n%s", deadStatus, currentCmd, startupCommand, paneText)
		}

		_, err = capturePane(session)
		if err == nil {
			if err := requireSessionAliveFor(session, startupSettleWindow); err != nil {
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
		return fmt.Errorf("waitForRuntimeReady: session %q not ready within %s: %w", session, timeout, lastErr)
	}
	return fmt.Errorf("waitForRuntimeReady: session %q not ready within %s", session, timeout)
}

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

func tmuxHasSession(session string) (bool, error) {
	err := runTmux("has-session", "-t", session)
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

func tmuxPaneState(session string) (bool, int, string, error) {
	cmd := exec.Command("tmux", tmuxArgs("list-panes", "-t", session, "-F", "#{pane_dead}\t#{pane_dead_status}\t#{pane_current_command}")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, 0, "", fmt.Errorf("tmuxPaneState: list-panes -t %s: %w (%s)", session, err, strings.TrimSpace(string(out)))
	}
	return parsePaneStateLine(string(out))
}

func parsePaneStateLine(raw string) (bool, int, string, error) {
	line := strings.TrimSpace(raw)
	if line == "" {
		return false, 0, "", errors.New("parsePaneStateLine: empty output from list-panes")
	}
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) < 3 {
		return false, 0, "", fmt.Errorf("parsePaneStateLine: unexpected format: %q", line)
	}
	dead := strings.TrimSpace(parts[0]) == "1"
	status := 0
	if s := strings.TrimSpace(parts[1]); s != "" {
		var err error
		status, err = strconv.Atoi(s)
		if err != nil {
			return false, 0, "", fmt.Errorf("parsePaneStateLine: parse status %q: %w", s, err)
		}
	}
	return dead, status, strings.TrimSpace(parts[2]), nil
}

// resolveStartupCommand validates the command string, resolves the binary
// to an absolute path via LookPath, and returns the normalised command.
// NOTE: The command is split on whitespace (strings.Fields), so quoted
// arguments containing spaces (e.g. --prompt "hello world") are not
// supported. Wrap such commands in a shell script if needed.
func resolveStartupCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("resolveStartupCommand: command is empty")
	}
	fields := strings.Fields(command)
	idx := -1
	for i, f := range fields {
		if isShellEnvAssignment(f) {
			continue
		}
		idx = i
		break
	}
	if idx < 0 {
		return "", errors.New("resolveStartupCommand: missing executable")
	}
	binPath, err := exec.LookPath(fields[idx])
	if err != nil {
		return "", fmt.Errorf("resolveStartupCommand: binary %q not found in PATH", fields[idx])
	}
	fields[idx] = binPath
	return strings.Join(fields, " "), nil
}

func isShellEnvAssignment(token string) bool {
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

func shouldRecoverSession(err error) bool {
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
	if err := runTmux("new-session", "-d", "-s", session, "-c", workDir, command); err != nil {
		return fmt.Errorf("createSession: new-session: %w", err)
	}
	if err := setTmuxServerOptions(); err != nil {
		return err
	}
	return configureSession(session)
}

func restartClaudeSession(session, workDir, command string) error {
	if err := runTmux("kill-session", "-t", session); err != nil {
		if !isTmuxNotFoundError(err) {
			return fmt.Errorf("restartClaudeSession: kill session: %w", err)
		}
	}
	if err := createSession(session, workDir, command); err != nil {
		return err
	}
	return waitForRuntimeReady(session, command, runtimeReadyTTL)
}

func capturePane(session string) (string, error) {
	cmd := exec.Command("tmux", tmuxArgs("capture-pane", "-p", "-t", session, "-S", "-")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("capturePane: capture-pane: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runTmux(args ...string) error {
	cmd := exec.Command("tmux", tmuxArgs(args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func tmuxArgs(args ...string) []string {
	if strings.TrimSpace(tmuxSocket) == "" {
		return args
	}
	return append([]string{"-L", tmuxSocket}, args...)
}

func cleanupSession(session string) {
	_ = runTmux("kill-session", "-t", session)
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func envBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	case "":
		return fallback
	default:
		fmt.Fprintf(os.Stderr, "warning: unrecognized boolean value %q for %s, using default %v\n", v, key, fallback)
		return fallback
	}
}

func validateSessionName(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return fmt.Errorf("invalid character %q in name %q; only alphanumeric, hyphens, and underscores are allowed", r, name)
		}
	}
	return nil
}
