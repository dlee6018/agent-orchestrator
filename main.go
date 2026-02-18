package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
var pollInterval = 500 * time.Millisecond         // 0.5s
var stableWindow = 2 * time.Second                // 2s
var startupSettleWindow = 1500 * time.Millisecond // 1.5s
var keystrokeSleep = 500 * time.Millisecond       // pause between text and Enter
var maxSendRetries = 2                            // 1 initial attempt + 1 retry

// Autonomous mode settings (var so tests can override).
var openRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"
var maxIterations = 0  // 0 means unlimited; overridden in tests
var maxFacts = 50      // memory compaction threshold; overridden via MEMORY_MAX_FACTS

const defaultOpenRouterModel = "anthropic/claude-opus-4.6"
const taskCompleteMarker = "TASK_COMPLETE"

//go:embed web/*
var webContent embed.FS

// ansiPattern matches ANSI escape sequences (CSI sequences and OSC sequences).
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\|\x1b\][^\x07]*\x07`)

// blankRunPattern matches 3+ consecutive blank lines.
var blankRunPattern = regexp.MustCompile(`(\n\s*){3,}`)

// OpenRouter API types.
type openRouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterRequest struct {
	Model       string              `json:"model"`
	Messages    []openRouterMessage `json:"messages"`
	Temperature float64             `json:"temperature"`
}

type openRouterChoice struct {
	Message      openRouterMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type openRouterUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openRouterResponse struct {
	ID      string             `json:"id"`
	Choices []openRouterChoice `json:"choices"`
	Usage   openRouterUsage    `json:"usage"`
}

type openRouterError struct {
	Error struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// Dashboard types.

type iterationEvent struct {
	Type         string      `json:"type"`                    // "task_info", "iteration_start", "iteration_end", "error", "complete"
	Iteration    int         `json:"iteration"`
	MaxIter      int         `json:"max_iter"`
	Timestamp    string      `json:"timestamp"`
	DurationMs   int64       `json:"duration_ms,omitempty"`
	Tokens       *tokenUsage `json:"tokens,omitempty"`
	Orchestrator string      `json:"orchestrator,omitempty"`
	ClaudeOutput string      `json:"claude_output,omitempty"`
	Error        string      `json:"error,omitempty"`
	Task         string      `json:"task,omitempty"`
	Model        string      `json:"model,omitempty"`
}

type tokenUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// sseBroker manages fan-out of SSE events to multiple connected clients.
// It retains the last task_info payload so late-connecting clients (or
// reconnects) immediately receive the current task metadata.
type sseBroker struct {
	mu           sync.Mutex
	clients      []chan string
	lastTaskInfo string // SSE payload for the most recent task_info event
}

func newSSEBroker() *sseBroker {
	return &sseBroker{}
}

// subscribe adds a new client and returns its event channel and an unsubscribe function.
// If a task_info event was previously published, it is replayed to the new client immediately.
func (b *sseBroker) subscribe() (<-chan string, func()) {
	ch := make(chan string, 64)
	b.mu.Lock()
	b.clients = append(b.clients, ch)
	// Replay the last task_info so late joiners see the task metadata.
	if b.lastTaskInfo != "" {
		select {
		case ch <- b.lastTaskInfo:
		default:
		}
	}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, c := range b.clients {
			if c == ch {
				b.clients = append(b.clients[:i], b.clients[i+1:]...)
				close(ch)
				return
			}
		}
	}
}

// publish sends an event to all connected clients (non-blocking).
// task_info events are retained so they can be replayed to late subscribers.
func (b *sseBroker) publish(event iterationEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	payload := fmt.Sprintf("data: %s\n\n", data)
	b.mu.Lock()
	defer b.mu.Unlock()
	if event.Type == "task_info" {
		b.lastTaskInfo = payload
	}
	for _, ch := range b.clients {
		select {
		case ch <- payload:
		default:
		}
	}
}

// emitEvent publishes an event to the SSE broker if it is non-nil.
func emitEvent(broker *sseBroker, event iterationEvent) {
	if broker != nil {
		broker.publish(event)
	}
}

// startDashboard starts the web dashboard HTTP server.
// It returns the address the server is listening on.
func startDashboard(broker *sseBroker, port int) (string, error) {
	mux := http.NewServeMux()

	webFS, err := fs.Sub(webContent, "web")
	if err != nil {
		return "", fmt.Errorf("startDashboard: fs.Sub: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(webFS)))

	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch, unsubscribe := broker.subscribe()
		defer unsubscribe()

		fmt.Fprintf(w, "data: {\"type\":\"connected\"}\n\n")
		flusher.Flush()

		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprint(w, msg)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("startDashboard: listen: %w", err)
	}

	actualAddr := listener.Addr().String()
	server := &http.Server{Handler: mux}
	go server.Serve(listener)

	return actualAddr, nil
}

// openBrowser attempts to open the URL in the default browser.
func openBrowser(url string) {
	_ = exec.Command("open", url).Start()
}

// loadEnvFile reads a .env file and sets any KEY=VALUE pairs as environment
// variables (only if not already set in the environment). Lines starting with
// '#' and blank lines are ignored.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no .env file is fine
		}
		return fmt.Errorf("loadEnvFile: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		// key=value format in .env
		key := strings.TrimSpace(line[:eq])
		value := strings.TrimSpace(line[eq+1:])
		// Strip matching surrounding quotes (single or double).
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		// Don't overwrite variables already set in the real environment.
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}
	return scanner.Err()
}

// main resolves config from env vars, sets up the tmux session, and enters the appropriate loop.
func main() {
	if err := loadEnvFile(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load .env: %v\n", err)
	}

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
	var workDir string
	if len(os.Args) > 1 {
		workDir, err = filepath.Abs(os.Args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to resolve working directory: %v\n", err)
			os.Exit(1)
		}
		info, err := os.Stat(workDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "working directory does not exist: %v\n", err)
			os.Exit(1)
		}
		if !info.IsDir() {
			fmt.Fprintf(os.Stderr, "working directory is not a directory: %s\n", workDir)
			os.Exit(1)
		}
	} else {
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to resolve working directory: %v\n", err)
			os.Exit(1)
		}
	}

	if err := ensureClaudeSession(session, workDir, command); err != nil {
		fmt.Fprintf(os.Stderr, "failed to prepare session: %v\n", err)
		os.Exit(1)
	}

	terminateOnQuit := envBool("TERMINATE_WHEN_QUIT", false)

	if envBool("AUTONOMOUS_MODE", true) {
		apiKey := os.Getenv("OPENROUTER_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(os.Stderr, "OPENROUTER_API_KEY is required in autonomous mode")
			os.Exit(1)
		}
		model := envOrDefault("OPENROUTER_MODEL", defaultOpenRouterModel)
		if v := os.Getenv("MAX_ITERATIONS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				maxIterations = n
			}
		}
		if v := os.Getenv("MEMORY_MAX_FACTS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				maxFacts = n
			}
		}

		fmt.Print("Enter task description: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
		if !scanner.Scan() {
			fmt.Fprintln(os.Stderr, "no task provided")
			os.Exit(1)
		}
		task := strings.TrimSpace(scanner.Text())
		if task == "" {
			fmt.Fprintln(os.Stderr, "empty task")
			os.Exit(1)
		}

		memories, memErr := loadMemory(workDir)
		if memErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to load memory: %v\n", memErr)
		} else if len(memories) > 0 {
			fmt.Printf("Loaded %d memory facts from %s\n", len(memories), memoryFileName)
		}

		var broker *sseBroker
		if envBool("DASHBOARD_ENABLED", true) {
			broker = newSSEBroker()
			dashPort := 0
			if v := os.Getenv("DASHBOARD_PORT"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n >= 0 {
					dashPort = n
				}
			}
			addr, dashErr := startDashboard(broker, dashPort)
			if dashErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to start dashboard: %v\n", dashErr)
				broker = nil
			} else {
				dashURL := fmt.Sprintf("http://%s", addr)
				fmt.Printf("Dashboard: %s\n", dashURL)
				if envBool("DASHBOARD_OPEN", true) {
					openBrowser(dashURL)
				}
			}
		}

		runWithCleanup(session, terminateOnQuit, func() {
			autonomousLoop(session, workDir, command, apiKey, model, task, broker, memories)
		})
	} else {
		fmt.Printf("Session %q is ready. Type messages and press Enter. Use /quit to exit.\n", session)
		runWithCleanup(session, terminateOnQuit, func() {
			chatLoop(session, workDir, command)
		})
	}
}

// runWithCleanup runs fn, optionally registering signal handlers and session cleanup.
func runWithCleanup(session string, terminate bool, fn func()) {
	if terminate {
		cleanup := func() { cleanupSession(session) }

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintln(os.Stderr, "\nsignal received, cleaning up tmux session...")
			cleanup()
			os.Exit(0)
		}()

		fn()
		cleanup()
	} else {
		fn()
	}
}

// chatLoop reads user input from stdin and sends each message to the tmux session.
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

// ensureClaudeSession creates a new tmux session or validates/restarts an existing one.
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

// configureSession sets remain-on-exit so dead panes stay around for diagnostics.
func configureSession(session string) error {
	// Keep the pane around if Claude exits so we can capture diagnostics.
	if err := runTmux("set-window-option", "-t", session, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("configureSession: set remain-on-exit: %w", err)
	}
	return nil
}

// sendAndCaptureWithRecovery sends a message and captures the response, retrying once on recoverable failures.
func sendAndCaptureWithRecovery(session, workDir, command, message, lastPane string) (string, error) {
	var lastErr error

	for attempt := 1; attempt <= maxSendRetries; attempt++ {
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

// sendMessage sends text to the tmux pane as literal keystrokes followed by Enter.
func sendMessage(session, message string) error {
	if err := runTmux("send-keys", "-t", session, "-l", message); err != nil {
		return fmt.Errorf("sendMessage: send-keys literal: %w", err)
	}
	time.Sleep(keystrokeSleep)
	if err := runTmux("send-keys", "-t", session, "C-m"); err != nil {
		return fmt.Errorf("sendMessage: send-keys enter: %w", err)
	}
	return nil
}

// waitForPaneUpdate polls the tmux pane until its content changes and stabilizes.
func waitForPaneUpdate(session, previous string, timeout time.Duration) (string, error) {
	return waitForPaneUpdateWithCapture(previous, timeout, func() (string, error) {
		return capturePane(session)
	}, func() (bool, error) {
		dead, _, _, err := tmuxPaneState(session)
		if err != nil {
			return false, err
		}
		return !dead, nil
	})
}

// waitForPaneUpdateWithCapture is the testable core of waitForPaneUpdate using injectable capture and checkAlive funcs.
func waitForPaneUpdateWithCapture(previous string, timeout time.Duration, capture func() (string, error), checkAlive func() (bool, error)) (string, error) {
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
		alive, aliveErr := checkAlive()
		if aliveErr != nil {
			return last, fmt.Errorf("waitForPaneUpdateWithCapture: liveness check failed: %w", aliveErr)
		}
		if !alive {
			return last, fmt.Errorf("waitForPaneUpdateWithCapture: claude code process is dead, no pane changes within %s", timeout)
		}
		return last, fmt.Errorf("waitForPaneUpdateWithCapture: claude code is still working, no pane changes within %s", timeout)
	}
	return last, fmt.Errorf("waitForPaneUpdateWithCapture: timeout (%s) reached, content changed but did not stabilize", timeout)
}

// waitForRuntimeReady blocks until the tmux session is alive and the startup command hasn't crashed.
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

// tmuxPaneState returns whether the pane is dead, its exit status, and current command.
func tmuxPaneState(session string) (bool, int, string, error) {
	cmd := exec.Command("tmux", tmuxArgs("list-panes", "-t", session, "-F", "#{pane_dead}\t#{pane_dead_status}\t#{pane_current_command}")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, 0, "", fmt.Errorf("tmuxPaneState: list-panes -t %s: %w (%s)", session, err, strings.TrimSpace(string(out)))
	}
	return parsePaneStateLine(string(out))
}

// parsePaneStateLine parses a tab-separated "dead\tstatus\tcommand" line from list-panes.
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

// isShellEnvAssignment returns true if the token looks like KEY=VALUE (e.g. "FOO=bar").
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

// shouldRecoverSession checks if an error indicates the tmux session/server is gone and worth restarting.
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

// restartClaudeSession kills the existing session and creates a fresh one.
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

// capturePane returns the full visible text of the tmux pane.
func capturePane(session string) (string, error) {
	cmd := exec.Command("tmux", tmuxArgs("capture-pane", "-p", "-t", session, "-S", "-")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("capturePane: capture-pane: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// runTmux executes a tmux command with the configured socket and returns any error.
func runTmux(args ...string) error {
	cmd := exec.Command("tmux", tmuxArgs(args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// tmuxArgs prepends the -L socket flag to isolate from the user's default tmux server.
func tmuxArgs(args ...string) []string {
	if strings.TrimSpace(tmuxSocket) == "" {
		return args
	}
	return append([]string{"-L", tmuxSocket}, args...)
}

// cleanupSession kills the tmux session, ignoring errors.
func cleanupSession(session string) {
	_ = runTmux("kill-session", "-t", session)
}

// envOrDefault returns the env var value for key, or fallback if unset/empty.
func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

// envBool parses a boolean env var (true/1/yes or false/0/no), returning fallback if unset.
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

// validateSessionName rejects names with characters outside [a-zA-Z0-9_-].
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

// ---------------------------------------------------------------------------
// Persistent memory
// ---------------------------------------------------------------------------

const memoryFileName = "memory.json"

// loadMemory reads the memory file from workDir and returns the stored facts.
// Returns nil, nil if the file does not exist.
func loadMemory(workDir string) ([]string, error) {
	path := filepath.Join(workDir, memoryFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("loadMemory: %w", err)
	}
	var facts []string
	if err := json.Unmarshal(data, &facts); err != nil {
		return nil, fmt.Errorf("loadMemory: unmarshal: %w", err)
	}
	return facts, nil
}

// saveMemory writes the facts to the memory file in workDir.
func saveMemory(workDir string, facts []string) error {
	if facts == nil {
		facts = []string{}
	}
	data, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return fmt.Errorf("saveMemory: marshal: %w", err)
	}
	path := filepath.Join(workDir, memoryFileName)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("saveMemory: write: %w", err)
	}
	return nil
}

// extractMemorySaves scans the LLM reply for lines matching "MEMORY_SAVE: <text>",
// collects them as new facts, and returns the cleaned reply with those lines removed.
func extractMemorySaves(reply string) ([]string, string) {
	var facts []string
	var kept []string
	for _, line := range strings.Split(reply, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "MEMORY_SAVE:"); ok {
			fact := strings.TrimSpace(after)
			if fact != "" {
				facts = append(facts, fact)
			}
		} else {
			kept = append(kept, line)
		}
	}
	return facts, strings.Join(kept, "\n")
}

// deduplicateMemory returns facts with duplicates removed, preserving order.
func deduplicateMemory(facts []string) []string {
	seen := make(map[string]bool, len(facts))
	var out []string
	for _, f := range facts {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// compactMemory asks the LLM to consolidate a list of facts into a shorter list.
// On failure it returns the original facts unchanged.
func compactMemory(apiKey, model string, facts []string) ([]string, error) {
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return facts, nil
	}
	prompt := fmt.Sprintf(`You are a memory compaction assistant. Below is a JSON array of facts from previous sessions. Consolidate them into a shorter list:
- Merge duplicate or near-duplicate entries
- Remove stale or irrelevant entries
- Combine related items into single entries
- Keep the most important and actionable facts

Return ONLY a valid JSON array of strings, nothing else.

Facts:
%s`, string(factsJSON))

	messages := []openRouterMessage{
		{Role: "user", Content: prompt},
	}
	reply, _, err := callOpenRouter(apiKey, model, messages, 0.2)
	if err != nil {
		return facts, fmt.Errorf("compactMemory: %w", err)
	}

	// Extract JSON array from the reply (handle possible markdown fences).
	cleaned := strings.TrimSpace(reply)
	if start := strings.Index(cleaned, "["); start >= 0 {
		if end := strings.LastIndex(cleaned, "]"); end > start {
			cleaned = cleaned[start : end+1]
		}
	}

	var compacted []string
	if err := json.Unmarshal([]byte(cleaned), &compacted); err != nil {
		return facts, fmt.Errorf("compactMemory: parse response: %w", err)
	}
	if len(compacted) == 0 {
		return facts, nil
	}
	return compacted, nil
}

// ---------------------------------------------------------------------------
// Autonomous mode
// ---------------------------------------------------------------------------

// callOpenRouter sends a chat completion request to the OpenRouter API
// and returns the assistant's reply content and token usage.
func callOpenRouter(apiKey, model string, messages []openRouterMessage, temperature float64) (string, openRouterUsage, error) {
	reqBody := openRouterRequest{
		Model:       model,
		Messages:    messages,
		Temperature: temperature,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", openRouterUsage{}, fmt.Errorf("callOpenRouter: marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", openRouterEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", openRouterUsage{}, fmt.Errorf("callOpenRouter: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", openRouterUsage{}, fmt.Errorf("callOpenRouter: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", openRouterUsage{}, fmt.Errorf("callOpenRouter: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr openRouterError
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
			return "", openRouterUsage{}, fmt.Errorf("callOpenRouter: API error %d: %s", resp.StatusCode, apiErr.Error.Message)
		}
		return "", openRouterUsage{}, fmt.Errorf("callOpenRouter: HTTP %d: %s", resp.StatusCode, truncateForLog(string(body), 200))
	}

	var result openRouterResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", openRouterUsage{}, fmt.Errorf("callOpenRouter: unmarshal response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", result.Usage, fmt.Errorf("callOpenRouter: empty choices in response")
	}
	return result.Choices[0].Message.Content, result.Usage, nil
}

// cleanPaneOutput strips ANSI escape sequences, collapses excessive blank
// lines, and trims trailing whitespace.
func cleanPaneOutput(raw string) string {
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

// buildSystemPrompt returns the system prompt for the orchestrator LLM.
func buildSystemPrompt(memories []string) string {
	base := `You are an autonomous agent driving a Claude Code CLI session via tmux.

Your responses are sent directly as keystrokes to the Claude Code terminal. Do NOT wrap your replies in markdown code fences or add commentary — type exactly what Claude Code should receive as input.

Rules:
- Each response you give will be typed into the Claude Code CLI and executed.
- After each response, you will see the tmux pane output showing Claude Code's reaction.
- Analyze the output carefully before deciding your next action.
- If Claude Code asks a question or needs confirmation, respond appropriately.
- If an approach fails, try a different strategy — do not repeat the same failed command.
- If Claude Code shows an error, read it carefully and adapt.
- Keep your inputs concise and focused on the task.
- After each action, suggest the next steps so there is always forward progress. Do not wait passively — proactively identify what should be done next and continue working.
- To save a fact for future sessions, include a line starting with "MEMORY_SAVE: " followed by the fact. These lines will be stripped before sending to Claude Code. Use this to remember project conventions, pitfalls, user preferences, or anything useful across sessions.

When the task is fully complete and you have verified the results, respond with exactly:
TASK_COMPLETE

Only send TASK_COMPLETE when you are confident the task is done. Do not send it prematurely.`

	if len(memories) > 0 {
		var sb strings.Builder
		sb.WriteString(base)
		sb.WriteString("\n\n## Memory from previous sessions\n")
		for _, fact := range memories {
			sb.WriteString("- ")
			sb.WriteString(fact)
			sb.WriteByte('\n')
		}
		return sb.String()
	}
	return base
}

// truncateForLog truncates s to maxLen characters, appending "..." if truncated.
func truncateForLog(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// autonomousLoop drives Claude Code via an LLM agent loop.
// It sends the task to the LLM, relays its decisions to Claude Code,
// and feeds back the pane output until the LLM signals TASK_COMPLETE.
// If maxIterations > 0 the loop stops after that many iterations.
// memories carries persistent facts from previous sessions; new facts
// are extracted from MEMORY_SAVE: lines and saved on exit.
func autonomousLoop(session, workDir, command, apiKey, model, task string, broker *sseBroker, memories []string) {
	fmt.Println("========================================")
	fmt.Println("AUTONOMOUS MODE")
	fmt.Printf("Model: %s\n", model)
	if maxIterations > 0 {
		fmt.Printf("Max iterations: %d\n", maxIterations)
	} else {
		fmt.Println("Max iterations: unlimited")
	}
	fmt.Printf("Task: %s\n", task)
	fmt.Println("========================================")

	emitEvent(broker, iterationEvent{
		Type:      "task_info",
		Timestamp: time.Now().Format(time.RFC3339),
		MaxIter:   maxIterations,
		Task:      task,
		Model:     model,
	})

	// Save memory on exit (deferred early so it runs on all exit paths).
	defer func() {
		if len(memories) > 0 {
			if err := saveMemory(workDir, memories); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to save memory: %v\n", err)
			} else {
				fmt.Printf("Saved %d memory facts to %s\n", len(memories), memoryFileName)
			}
		}
	}()

	messages := []openRouterMessage{
		{Role: "system", Content: buildSystemPrompt(memories)},
		{Role: "user", Content: fmt.Sprintf("Task: %s\n\nYou are now connected to the Claude Code CLI. Send your first message to begin working on the task.", task)},
	}

	lastPane := ""
	consecutiveAPIErrors := 0

	for i := 1; maxIterations == 0 || i <= maxIterations; i++ {
		iterStart := time.Now()

		if maxIterations > 0 {
			fmt.Printf("\n┌─── Iteration %d/%d ───────────────────────\n", i, maxIterations)
		} else {
			fmt.Printf("\n┌─── Iteration %d ─────────────────────────\n", i)
		}

		emitEvent(broker, iterationEvent{
			Type:      "iteration_start",
			Iteration: i,
			MaxIter:   maxIterations,
			Timestamp: iterStart.Format(time.RFC3339),
		})

		// Compact memory if it exceeds the threshold.
		if len(memories) > maxFacts {
			fmt.Printf("│ Memory has %d facts (threshold %d), compacting...\n", len(memories), maxFacts)
			compacted, compactErr := compactMemory(apiKey, model, memories)
			if compactErr != nil {
				fmt.Fprintf(os.Stderr, "│ Memory compaction failed (non-fatal): %v\n", compactErr)
			} else {
				fmt.Printf("│ Compacted memory: %d → %d facts\n", len(memories), len(compacted))
				memories = compacted
				// Rebuild system prompt with compacted memories.
				messages[0] = openRouterMessage{Role: "system", Content: buildSystemPrompt(memories)}
			}
		}

		// Call the orchestrator LLM.
		reply, usage, err := callOpenRouter(apiKey, model, messages, 0.3)
		if err != nil {
			consecutiveAPIErrors++
			fmt.Fprintf(os.Stderr, "│ API ERROR (%d/3): %v\n", consecutiveAPIErrors, err)
			emitEvent(broker, iterationEvent{
				Type:      "error",
				Iteration: i,
				Timestamp: time.Now().Format(time.RFC3339),
				Error:     fmt.Sprintf("API error (%d/3): %v", consecutiveAPIErrors, err),
			})
			if consecutiveAPIErrors >= 3 {
				fmt.Fprintln(os.Stderr, "│ Too many consecutive API errors, aborting.")
				emitEvent(broker, iterationEvent{
					Type:      "complete",
					Iteration: i,
					Timestamp: time.Now().Format(time.RFC3339),
					Error:     "aborted after 3 consecutive API errors",
				})
				return
			}
			fmt.Fprintln(os.Stderr, "│ Retrying in 5s...")
			time.Sleep(5 * time.Second)
			if maxIterations > 0 {
				i-- // Don't count API errors toward iteration limit.
			}
			continue
		}
		consecutiveAPIErrors = 0

		fmt.Printf("│ Tokens: prompt=%d completion=%d total=%d\n", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)

		// Log the LLM's decision.
		fmt.Println("│")
		fmt.Println("│ ╔══ ORCHESTRATOR → CLAUDE CODE ══════════")
		for _, line := range strings.Split(reply, "\n") {
			fmt.Printf("│ ║ %s\n", line)
		}
		fmt.Println("│ ╚════════════════════════════════════════")

		// Extract memory saves from the reply.
		newFacts, cleanedReply := extractMemorySaves(reply)
		if len(newFacts) > 0 {
			memories = append(memories, newFacts...)
			memories = deduplicateMemory(memories)
			fmt.Printf("│ Saved %d new memory fact(s) (total: %d)\n", len(newFacts), len(memories))
			reply = cleanedReply
		}

		// Check for task completion.
		if strings.Contains(reply, taskCompleteMarker) {
			fmt.Println("│")
			fmt.Println("│ *** TASK COMPLETE ***")
			fmt.Printf("└─── Finished after %d iterations ────────\n", i)
			messages = append(messages, openRouterMessage{Role: "assistant", Content: reply})
			emitEvent(broker, iterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    maxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &tokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
			})
			emitEvent(broker, iterationEvent{
				Type:      "complete",
				Iteration: i,
				Timestamp: time.Now().Format(time.RFC3339),
				Task:      task,
			})
			return
		}

		// Send the LLM's reply to Claude Code.
		pane, err := sendAndCaptureWithRecovery(session, workDir, command, reply, lastPane)

		// If Claude Code is still working, keep polling instead of calling the LLM.
		for err != nil && strings.Contains(err.Error(), "claude code is still working") {
			fmt.Println("│ Claude Code is still working, waiting for output...")
			lastPane = pane
			pane, err = waitForPaneUpdate(session, lastPane, 90*time.Second)
		}

		if err != nil {
			errMsg := fmt.Sprintf("Error sending to Claude Code: %v", err)
			fmt.Fprintf(os.Stderr, "│ TMUX ERROR: %v\n", err)
			emitEvent(broker, iterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    maxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &tokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
				Error:        fmt.Sprintf("tmux error: %v", err),
			})
			// Feed the error back so the LLM can adapt.
			messages = append(messages,
				openRouterMessage{Role: "assistant", Content: reply},
				openRouterMessage{Role: "user", Content: errMsg},
			)
			continue
		}

		cleaned := cleanPaneOutput(pane)

		// Log Claude Code's response.
		fmt.Println("│")
		fmt.Println("│ ╔══ CLAUDE CODE OUTPUT ══════════════════")
		for _, line := range strings.Split(cleaned, "\n") {
			fmt.Printf("│ ║ %s\n", line)
		}
		fmt.Println("│ ╚════════════════════════════════════════")
		fmt.Printf("└─────────────────────────────────────────\n")

		emitEvent(broker, iterationEvent{
			Type:       "iteration_end",
			Iteration:  i,
			MaxIter:    maxIterations,
			Timestamp:  time.Now().Format(time.RFC3339),
			DurationMs: time.Since(iterStart).Milliseconds(),
			Tokens: &tokenUsage{
				Prompt:     usage.PromptTokens,
				Completion: usage.CompletionTokens,
				Total:      usage.TotalTokens,
			},
			Orchestrator: reply,
			ClaudeOutput: cleaned,
		})

		// Append to conversation history.
		messages = append(messages,
			openRouterMessage{Role: "assistant", Content: reply},
			openRouterMessage{Role: "user", Content: fmt.Sprintf("Claude Code output:\n%s", cleaned)},
		)
		lastPane = pane
	}

	fmt.Fprintf(os.Stderr, "\nReached maximum iterations (%d) without task completion.\n", maxIterations)
	emitEvent(broker, iterationEvent{
		Type:      "complete",
		Iteration: maxIterations,
		Timestamp: time.Now().Format(time.RFC3339),
		Error:     fmt.Sprintf("reached maximum iterations (%d) without task completion", maxIterations),
	})
}
