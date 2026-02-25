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
	"time"

	"github.com/dlee6018/agent-orchestrator/config"
	"github.com/dlee6018/agent-orchestrator/dashboard"
	"github.com/dlee6018/agent-orchestrator/helpers"
	"github.com/dlee6018/agent-orchestrator/memory"
	"github.com/dlee6018/agent-orchestrator/orchestrator"
	"github.com/dlee6018/agent-orchestrator/tmux"
)

const (
	defaultSession = "gt-claude-loop"
	defaultSocket  = "gt-claude-loop"
)

// HealthPollInterval is how often the health poller checks agent sessions.
var HealthPollInterval = 5 * time.Second

// main resolves config from env vars, sets up the tmux session, and enters the appropriate loop.
func main() {
	// Load config.json (user's /setup choices) from global config dir — highest precedence.
	if cfgDir, cfgDirErr := config.ConfigDir(); cfgDirErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not determine config directory: %v\n", cfgDirErr)
	} else if cfg, cfgErr := config.LoadConfig(cfgDir); cfgErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to load config.json: %v\n", cfgErr)
	} else if cfg != nil {
		config.ApplyConfig(cfg)
		fmt.Println("Loaded settings from config.json")
	}

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
	defaultModel := helpers.EnvOrDefault("DEFAULT_MODEL", "claude")
	agentCommand, agentName := helpers.ResolveAgentConfig(defaultModel)
	command, err := tmux.ResolveStartupCommand(helpers.EnvOrDefault("CLAUDE_CMD", agentCommand))
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

	if helpers.EnvBool("AUTONOMOUS_MODE", true) {
		fmt.Print("Press /setup to setup first. \n")
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

		// Handle /setup before requiring API key.
		if task == "/setup" {
			if err := handleSetup(scanner); err != nil {
				fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
				os.Exit(1)
			}
			return
		}

		apiKey := os.Getenv("OPENROUTER_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(os.Stderr, "OPENROUTER_API_KEY is required in autonomous mode")
			os.Exit(1)
		}
		model := helpers.EnvOrDefault("OPENROUTER_MODEL", orchestrator.DefaultModel)
		orchestrator.Provider = helpers.EnvOrDefault("OPENROUTER_PROVIDER", orchestrator.Provider)
		// Force Bedrock provider when DEFAULT_MODEL starts with "bedrock".
		if strings.HasPrefix(strings.ToLower(defaultModel), "bedrock") && os.Getenv("OPENROUTER_PROVIDER") == "" {
			orchestrator.Provider = "amazon-bedrock"
			fmt.Printf("Auto-detected Bedrock model (%s), setting provider to amazon-bedrock\n", defaultModel)
		}
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
			restartFn := func(session string) error {
				return tmux.EnsureClaudeSession(session, workDir, command)
			}
			addr, dashErr := dashboard.StartDashboard(broker, dashPort, restartFn)
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

		if helpers.EnvBool("MULTI_AGENT_MODE", false) {
			if v := os.Getenv("CONTEXT_SUMMARY_THRESHOLD"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					orchestrator.ContextSummaryThreshold = n
				}
			}
			if v := os.Getenv("CONTEXT_KEEP_RECENT"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					orchestrator.ContextKeepRecent = n
				}
			}
			// Create 3 tmux sessions: {session}-planner, {session}-executor, {session}-verifier
			roles := []orchestrator.AgentRole{orchestrator.AgentPlanner, orchestrator.AgentExecutor, orchestrator.AgentVerifier}
			agents := make([]orchestrator.AgentState, len(roles))
			sessions := make([]string, len(roles))
			for i, role := range roles {
				sessName := fmt.Sprintf("%s-%s", session, string(role))
				sessions[i] = sessName
				if err := tmux.EnsureClaudeSession(sessName, workDir, command); err != nil {
					fmt.Fprintf(os.Stderr, "failed to prepare %s session: %v\n", role, err)
					os.Exit(1)
				}
				agents[i] = orchestrator.AgentState{
					Role:    role,
					Session: sessName,
				}
			}
			roleNames := make([]string, len(roles))
			for i, r := range roles {
				roleNames[i] = string(r)
			}
			stopHealth := startHealthPoller(sessions, roleNames, broker)
			runWithCleanup(sessions, func() {
				defer stopHealth()
				orchestrator.MultiAgentLoop(agents, workDir, command, apiKey, model, agentName, defaultModel, task, broker, memories)
			})
		} else {
			if err := tmux.EnsureClaudeSession(session, workDir, command); err != nil {
				fmt.Fprintf(os.Stderr, "failed to prepare session: %v\n", err)
				os.Exit(1)
			}
			stopHealth := startHealthPoller([]string{session}, []string{"agent"}, broker)
			runWithCleanup([]string{session}, func() {
				defer stopHealth()
				orchestrator.AutonomousLoop(session, workDir, command, apiKey, model, task, agentName, defaultModel, broker, memories)
			})
		}
	} else {
		if err := tmux.EnsureClaudeSession(session, workDir, command); err != nil {
			fmt.Fprintf(os.Stderr, "failed to prepare session: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Session %q is ready. Type messages and press Enter. Use /quit to exit.\n", session)
		runWithCleanup([]string{session}, func() {
			chatLoop(session, workDir, command)
		})
	}
}

// runWithCleanup runs fn with signal handlers and session cleanup.
// sessions is a list of tmux session names to clean up (supports both single and multi-agent modes).
func runWithCleanup(sessions []string, fn func()) {
	cleanup := func() {
		for _, s := range sessions {
			tmux.CleanupSession(s)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nsignal received, cleaning up tmux session(s)...")
		cleanup()
		os.Exit(0)
	}()

	fn()
	cleanup()
}

// startHealthPoller starts a goroutine that periodically checks agent session health
// and publishes health_status events to the SSE broker. Returns a stop function.
func startHealthPoller(sessions, roles []string, broker *dashboard.SSEBroker) func() {
	if broker == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(HealthPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				agents := make([]dashboard.AgentHealthStatus, len(sessions))
				for i, sess := range sessions {
					h := tmux.CheckPaneHealth(sess)
					role := ""
					if i < len(roles) {
						role = roles[i]
					}
					agents[i] = dashboard.AgentHealthStatus{
						Role:       role,
						Session:    sess,
						Alive:      h.Alive,
						ExitStatus: h.ExitStatus,
					}
				}
				broker.Publish(dashboard.IterationEvent{
					Type:      "health_status",
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					Agents:    agents,
				})
			}
		}
	}()
	return func() { close(done) }
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
		if message == "/setup" {
			if err := handleSetup(scanner); err != nil {
				fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
			}
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

// handleSetup runs the interactive /setup wizard and saves the result to the global config directory.
// Returns an error instead of calling os.Exit so callers can perform cleanup (e.g. tmux sessions).
func handleSetup(scanner *bufio.Scanner) error {
	cfg, err := config.RunSetupWizard(scanner, os.Stdout)
	if err != nil {
		return fmt.Errorf("handleSetup: wizard: %w", err)
	}
	cfgDir, err := config.ConfigDir()
	if err != nil {
		return fmt.Errorf("handleSetup: config dir: %w", err)
	}
	if err := config.SaveConfig(cfgDir, cfg); err != nil {
		return fmt.Errorf("handleSetup: save: %w", err)
	}
	fmt.Printf("Configuration saved to %s. Restart to apply.\n", filepath.Join(cfgDir, config.FileName))
	return nil
}
