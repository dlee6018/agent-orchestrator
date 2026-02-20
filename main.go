package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/dlee6018/agent-orchestrator/dashboard"
	"github.com/dlee6018/agent-orchestrator/helpers"
	"github.com/dlee6018/agent-orchestrator/memory"
	"github.com/dlee6018/agent-orchestrator/orchestrator"
	"github.com/dlee6018/agent-orchestrator/tmux"
)

const (
	defaultSession = "gt-claude-loop"
	defaultSocket  = "gt-claude-loop"
	defaultCommand = "claude --dangerously-skip-permissions --setting-sources user"
)

// main resolves config from env vars, sets up the tmux session, and enters the appropriate loop.
func main() {
	if err := helpers.LoadEnvFile(".env"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load .env: %v\n", err)
	}

	session := helpers.EnvOrDefault("CLAUDE_TMUX_SESSION", defaultSession)
	tmux.Socket = helpers.EnvOrDefault("CLAUDE_TMUX_SOCKET", defaultSocket)
	if err := helpers.ValidateSessionName(session); err != nil {
		fmt.Fprintf(os.Stderr, "invalid session name: %v\n", err)
		os.Exit(1)
	}
	if err := helpers.ValidateSessionName(tmux.Socket); err != nil {
		fmt.Fprintf(os.Stderr, "invalid socket name: %v\n", err)
		os.Exit(1)
	}
	command, err := tmux.ResolveStartupCommand(helpers.EnvOrDefault("CLAUDE_CMD", defaultCommand))
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

	if err := tmux.EnsureClaudeSession(session, workDir, command); err != nil {
		fmt.Fprintf(os.Stderr, "failed to prepare session: %v\n", err)
		os.Exit(1)
	}

	terminateOnQuit := helpers.EnvBool("TERMINATE_WHEN_QUIT", false)

	if helpers.EnvBool("AUTONOMOUS_MODE", true) {
		apiKey := os.Getenv("OPENROUTER_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(os.Stderr, "OPENROUTER_API_KEY is required in autonomous mode")
			os.Exit(1)
		}
		model := helpers.EnvOrDefault("OPENROUTER_MODEL", orchestrator.DefaultModel)
		if v := os.Getenv("MAX_ITERATIONS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				orchestrator.MaxIterations = n
			}
		}
		if v := os.Getenv("MEMORY_MAX_FACTS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				memory.MaxFacts = n
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

		memories, memErr := memory.LoadMemory(workDir)
		if memErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to load memory: %v\n", memErr)
		} else if len(memories) > 0 {
			fmt.Printf("Loaded %d memory facts from %s\n", len(memories), memory.FileName)
		}

		var broker *dashboard.SSEBroker
		if helpers.EnvBool("DASHBOARD_ENABLED", true) {
			broker = dashboard.NewSSEBroker()
			dashPort := 0
			if v := os.Getenv("DASHBOARD_PORT"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n >= 0 {
					dashPort = n
				}
			}
			addr, dashErr := dashboard.StartDashboard(broker, dashPort)
			if dashErr != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to start dashboard: %v\n", dashErr)
				broker = nil
			} else {
				dashURL := fmt.Sprintf("http://%s", addr)
				fmt.Printf("Dashboard: %s\n", dashURL)
				if helpers.EnvBool("DASHBOARD_OPEN", true) {
					dashboard.OpenBrowser(dashURL)
				}
			}
		}

		runWithCleanup(session, terminateOnQuit, func() {
			orchestrator.AutonomousLoop(session, workDir, command, apiKey, model, task, broker, memories)
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
		cleanup := func() { tmux.CleanupSession(session) }

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

		pane, err := tmux.SendAndCaptureWithRecovery(session, workDir, command, message, lastPane)
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
