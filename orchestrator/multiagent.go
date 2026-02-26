package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dlee6018/agent-orchestrator/dashboard"
	"github.com/dlee6018/agent-orchestrator/memory"
	"github.com/dlee6018/agent-orchestrator/tmux"
)

// ContextSummaryThreshold is the message count before older context is summarized.
var ContextSummaryThreshold = 30

// ContextKeepRecent is how many recent messages to preserve when summarizing context.
var ContextKeepRecent = 10

// SendToAgentTimeout is the overall deadline for a sendToAgent goroutine
// to prevent goroutine leaks if tmux becomes unresponsive.
var SendToAgentTimeout = 10 * time.Minute

// MultiAgentLoop drives three agent CLI sessions (planner, executor, verifier)
// via an LLM orchestrator loop. The LLM emits JSON directives to target individual
// agents. Agents run concurrently in goroutines; messages to busy agents are queued
// (max 1 per agent). The loop exits when the LLM signals TASK_COMPLETE or
// MaxIterations is reached.
// agentModel is the inner agent's model name (DEFAULT_MODEL, e.g. "gpt-5.3-codex").
func MultiAgentLoop(agents []AgentState, workDir, command, apiKey, model, agentName, agentModel, task string, broker *dashboard.SSEBroker, memories []string) {
	fmt.Println("========================================")
	fmt.Println("MULTI-AGENT MODE")
	fmt.Printf("Model: %s\n", model)
	if MaxIterations > 0 {
		fmt.Printf("Max iterations: %d\n", MaxIterations)
	} else {
		fmt.Println("Max iterations: unlimited")
	}
	fmt.Printf("Task: %s\n", task)
	fmt.Printf("Agents: planner, executor, verifier (each running %s)\n", agentName)
	fmt.Println("========================================")

	broker.Publish(dashboard.IterationEvent{
		Type:       "task_info",
		Timestamp:  time.Now().Format(time.RFC3339),
		MaxIter:    MaxIterations,
		Task:       task,
		Model:      model,
		AgentModel: agentModel,
		Agent:      agentName,
		Mode:       "multi-agent",
	})

	// Save memory on exit (deferred early so it runs on all exit paths).
	defer func() {
		if len(memories) > 0 {
			if err := memory.SaveMemory(workDir, memories); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to save memory: %v\n", err)
			} else {
				fmt.Printf("Saved %d memory facts to %s\n", len(memories), memory.FileName)
			}
		}
	}()

	messages := []Message{
		{Role: "system", Content: BuildMultiAgentSystemPrompt(agentName, memories)},
		{Role: "user", Content: fmt.Sprintf("Task: %s\n\nYou are now connected to 3 %s CLI sessions (planner, executor, verifier). Send a JSON directive to begin working on the task.", task, agentName)},
	}

	resultCh := make(chan agentResult, 3)
	consecutiveAPIErrors := 0
	consecutiveParseErrors := 0

	for i := 1; MaxIterations == 0 || i <= MaxIterations; i++ {
		iterStart := time.Now()

		if MaxIterations > 0 {
			fmt.Printf("\n┌─── Iteration %d/%d ───────────────────────\n", i, MaxIterations)
		} else {
			fmt.Printf("\n┌─── Iteration %d ─────────────────────────\n", i)
		}

		broker.Publish(dashboard.IterationEvent{
			Type:      "iteration_start",
			Iteration: i,
			MaxIter:   MaxIterations,
			Timestamp: iterStart.Format(time.RFC3339),
			Mode:      "multi-agent",
		})

		// Step a: Drain all completed agent results (non-blocking).
		var completedResults []agentResult
		draining := true
		for draining {
			select {
			case res := <-resultCh:
				completedResults = append(completedResults, res)
				// Mark the agent idle.
				if ag, err := findAgent(agents, string(res.Role)); err == nil {
					ag.Status = statusIdle
					ag.LastPane = res.Pane
				}
			default:
				draining = false
			}
		}

		// Step b: Process queues — for any newly idle agent with a queued message, send it.
		for idx := range agents {
			ag := &agents[idx]
			if ag.Status == statusIdle && ag.QueuedMsg != "" {
				msg := ag.QueuedMsg
				ag.QueuedMsg = ""
				sendToAgent(ag, msg, workDir, command, resultCh)
				fmt.Printf("│ Dequeued message for %s\n", ag.Role)
			}
		}

		// Step c: Build feedback from collected results.
		var feedback strings.Builder
		for _, res := range completedResults {
			if res.Err != nil {
				feedback.WriteString(fmt.Sprintf("[%s] error: %v\n", res.Role, res.Err))
			} else {
				feedback.WriteString(fmt.Sprintf("[%s] output:\n%s\n", res.Role, res.Output))
			}
		}

		// Step d: If no results and all targeted agents are busy, block-wait for one result.
		if len(completedResults) == 0 && allBusy(agents) {
			fmt.Println("│ All agents busy, waiting for a result...")
			res := <-resultCh
			if ag, err := findAgent(agents, string(res.Role)); err == nil {
				ag.Status = statusIdle
				ag.LastPane = res.Pane
			}
			// Process queue for newly idle agent.
			if ag, err := findAgent(agents, string(res.Role)); err == nil && ag.QueuedMsg != "" {
				msg := ag.QueuedMsg
				ag.QueuedMsg = ""
				sendToAgent(ag, msg, workDir, command, resultCh)
				fmt.Printf("│ Dequeued message for %s\n", ag.Role)
			}
			if res.Err != nil {
				feedback.WriteString(fmt.Sprintf("[%s] error: %v\n", res.Role, res.Err))
			} else {
				feedback.WriteString(fmt.Sprintf("[%s] output:\n%s\n", res.Role, res.Output))
			}
		}

		// Step e: If feedback available, append to messages.
		if feedback.Len() > 0 {
			messages = append(messages, Message{Role: "user", Content: feedback.String()})
		}

		// Step f: Context summarization.
		messages = summarizeContext(messages, apiKey, model)

		// Compact memory if it exceeds the threshold.
		if len(memories) > memory.MaxFacts {
			fmt.Printf("│ Memory has %d facts (threshold %d), compacting...\n", len(memories), memory.MaxFacts)
			compactFn := func(prompt string) (string, error) {
				msgs := []Message{{Role: "user", Content: prompt}}
				reply, _, err := CallOpenRouter(apiKey, model, msgs, 0.2)
				return reply, err
			}
			compacted, compactErr := memory.CompactMemory(compactFn, memories)
			if compactErr != nil {
				fmt.Fprintf(os.Stderr, "│ Memory compaction failed (non-fatal): %v\n", compactErr)
			} else {
				fmt.Printf("│ Compacted memory: %d → %d facts\n", len(memories), len(compacted))
				memories = compacted
				messages[0] = Message{Role: "system", Content: BuildMultiAgentSystemPrompt(agentName, memories)}
			}
		}

		// Ensure messages end with a user message (required by some providers like Bedrock).
		if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
			messages = append(messages, Message{Role: "user", Content: "Continue. What is your next directive?"})
		}

		// Step g: Call LLM.
		reply, usage, err := CallOpenRouter(apiKey, model, messages, 0.3)
		if err != nil {
			consecutiveAPIErrors++
			fmt.Fprintf(os.Stderr, "│ API ERROR (%d/3): %v\n", consecutiveAPIErrors, err)
			broker.Publish(dashboard.IterationEvent{
				Type:      "error",
				Iteration: i,
				Timestamp: time.Now().Format(time.RFC3339),
				Error:     fmt.Sprintf("API error (%d/3): %v", consecutiveAPIErrors, err),
				Mode:      "multi-agent",
			})
			if consecutiveAPIErrors >= 3 {
				fmt.Fprintln(os.Stderr, "│ Too many consecutive API errors, aborting.")
				broker.Publish(dashboard.IterationEvent{
					Type:      "complete",
					Iteration: i,
					Timestamp: time.Now().Format(time.RFC3339),
					Error:     "aborted after 3 consecutive API errors",
					Mode:      "multi-agent",
				})
				return
			}
			fmt.Fprintln(os.Stderr, "│ Retrying in 5s...")
			time.Sleep(5 * time.Second)
			if MaxIterations > 0 {
				// Decrement i so the subsequent loop increment (i++) restores it
				// to its pre-iteration value, effectively not counting this API
				// error toward the iteration limit.
				i--
			}
			continue
		}
		consecutiveAPIErrors = 0

		fmt.Printf("│ Tokens: prompt=%d completion=%d total=%d\n", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)

		// Log the LLM's decision.
		fmt.Println("│")
		fmt.Println("│ ╔══ ORCHESTRATOR ══════════════════════")
		for _, line := range strings.Split(reply, "\n") {
			fmt.Printf("│ ║ %s\n", line)
		}
		fmt.Println("│ ╚════════════════════════════════════════")

		// Step h: Extract memory saves from the reply.
		newFacts, cleanedReply := memory.ExtractMemorySaves(reply)
		if len(newFacts) > 0 {
			memories = append(memories, newFacts...)
			memories = memory.DeduplicateMemory(memories)
			fmt.Printf("│ Saved %d new memory fact(s) (total: %d)\n", len(newFacts), len(memories))
			reply = cleanedReply
		}

		// Step i: Check for task completion (before JSON parsing).
		if strings.Contains(reply, TaskCompleteMarker) {
			fmt.Println("│")
			fmt.Println("│ *** TASK COMPLETE ***")
			fmt.Printf("└─── Finished after %d iterations ────────\n", i)
			messages = append(messages, Message{Role: "assistant", Content: reply})
			broker.Publish(dashboard.IterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    MaxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &dashboard.TokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
				MemoryFacts:  newFacts,
				Mode:         "multi-agent",
			})
			broker.Publish(dashboard.IterationEvent{
				Type:      "complete",
				Iteration: i,
				Timestamp: time.Now().Format(time.RFC3339),
				Task:      task,
				Mode:      "multi-agent",
			})
			return
		}

		// Step j: Parse JSON directive.
		directive, parseErr := parseAgentDirective(reply)
		if parseErr != nil {
			consecutiveParseErrors++
			errMsg := fmt.Sprintf("Failed to parse directive (attempt %d/5): %v. Please respond with a JSON object: {\"agent\":\"planner|executor|verifier\",\"message\":\"...\"}", consecutiveParseErrors, parseErr)
			fmt.Fprintf(os.Stderr, "│ PARSE ERROR: %v\n", parseErr)

			broker.Publish(dashboard.IterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    MaxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &dashboard.TokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
				MemoryFacts:  newFacts,
				Error:        errMsg,
				Mode:         "multi-agent",
			})

			if consecutiveParseErrors >= 5 {
				fmt.Fprintln(os.Stderr, "│ Too many consecutive parse errors, aborting.")
				broker.Publish(dashboard.IterationEvent{
					Type:      "complete",
					Iteration: i,
					Timestamp: time.Now().Format(time.RFC3339),
					Error:     "aborted after 5 consecutive parse errors",
					Mode:      "multi-agent",
				})
				return
			}
			messages = append(messages,
				Message{Role: "assistant", Content: reply},
				Message{Role: "user", Content: errMsg},
			)
			continue
		}
		consecutiveParseErrors = 0

		// Step k: Route to agent.
		ag, findErr := findAgent(agents, directive.Agent)
		if findErr != nil {
			errMsg := fmt.Sprintf("Unknown agent %q. Valid agents are: planner, executor, verifier. Please try again.", directive.Agent)
			fmt.Fprintf(os.Stderr, "│ ROUTING ERROR: %v\n", findErr)

			broker.Publish(dashboard.IterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    MaxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &dashboard.TokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
				MemoryFacts:  newFacts,
				Error:        errMsg,
				Mode:         "multi-agent",
			})

			messages = append(messages,
				Message{Role: "assistant", Content: reply},
				Message{Role: "user", Content: errMsg},
			)
			continue
		}

		fmt.Printf("│ Routing to %s: %s\n", ag.Role, tmux.TruncateForLog(directive.Message, 100))
		fmt.Printf("└─────────────────────────────────────────\n")

		if ag.Status == statusIdle {
			// Target is idle — send via goroutine, mark busy.
			sendToAgent(ag, directive.Message, workDir, command, resultCh)
			messages = append(messages, Message{Role: "assistant", Content: reply})

			broker.Publish(dashboard.IterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    MaxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &dashboard.TokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
				MemoryFacts:  newFacts,
				Agent:        directive.Agent,
				Mode:         "multi-agent",
			})
		} else if ag.QueuedMsg == "" {
			// Target is busy, no queue — queue the message.
			ag.QueuedMsg = directive.Message
			queueMsg := fmt.Sprintf("%s is busy, your message has been queued (1 allowed). It will be delivered when %s finishes. Please address another agent.", ag.Role, ag.Role)
			fmt.Printf("│ Queued message for busy agent %s\n", ag.Role)

			broker.Publish(dashboard.IterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    MaxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &dashboard.TokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
				MemoryFacts:  newFacts,
				Agent:        directive.Agent,
				Mode:         "multi-agent",
			})

			messages = append(messages,
				Message{Role: "assistant", Content: reply},
				Message{Role: "user", Content: queueMsg},
			)
		} else {
			// Target is busy and queue is full.
			rejectMsg := fmt.Sprintf("%s is busy and already has a queued message. Please address a different agent (planner, executor, or verifier).", ag.Role)
			fmt.Printf("│ Queue full for busy agent %s\n", ag.Role)

			broker.Publish(dashboard.IterationEvent{
				Type:       "iteration_end",
				Iteration:  i,
				MaxIter:    MaxIterations,
				Timestamp:  time.Now().Format(time.RFC3339),
				DurationMs: time.Since(iterStart).Milliseconds(),
				Tokens: &dashboard.TokenUsage{
					Prompt:     usage.PromptTokens,
					Completion: usage.CompletionTokens,
					Total:      usage.TotalTokens,
				},
				Orchestrator: reply,
				MemoryFacts:  newFacts,
				Agent:        directive.Agent,
				Error:        rejectMsg,
				Mode:         "multi-agent",
			})

			messages = append(messages,
				Message{Role: "assistant", Content: reply},
				Message{Role: "user", Content: rejectMsg},
			)
		}
	}

	fmt.Fprintf(os.Stderr, "\nReached maximum iterations (%d) without task completion.\n", MaxIterations)
	broker.Publish(dashboard.IterationEvent{
		Type:      "complete",
		Iteration: MaxIterations,
		Timestamp: time.Now().Format(time.RFC3339),
		Error:     fmt.Sprintf("reached maximum iterations (%d) without task completion", MaxIterations),
		Mode:      "multi-agent",
	})
}

// BuildMultiAgentSystemPrompt returns the system prompt for the multi-agent orchestrator.
// agentName is the display name of the inner coding agent (e.g. "Claude Code", "Codex").
func BuildMultiAgentSystemPrompt(agentName string, memories []string) string {
	base := fmt.Sprintf(`You are an orchestrator coordinating three %s CLI sessions via tmux.

## Your role

You are a HIGH-LEVEL COORDINATOR. Each agent is an AI-powered %s session that understands natural language. Your job is to THINK about what needs to be done and give agents clear, high-level instructions in plain English. You are NOT a coding agent — do NOT type raw terminal commands, file paths, or shell operations. The agents will figure out which commands to run and how to accomplish the task.

You have 3 agent sessions available:
- **planner**: A %s session for planning, analysis, and architecture decisions.
- **executor**: A %s session for writing code, running commands, and making changes.
- **verifier**: A %s session for running tests, reviewing code, and validating changes.

## How to communicate

Send a JSON directive to target an agent:
{"agent":"planner","message":"analyze the codebase structure and identify the main modules"}
{"agent":"executor","message":"create a REST endpoint for user authentication using JWT"}
{"agent":"verifier","message":"run the full test suite and report any failures"}

GOOD messages (high-level, natural language):
- "Read the combat.js file and explain how the damage calculation works"
- "Refactor the database module to use connection pooling"
- "Find all files related to payment processing and summarize the flow"

BAD messages (raw commands — DO NOT do this):
- "cat ~/Desktop/project/src/combat.js"
- "grep -r 'function' src/"
- "npm test && echo done"

## Rules

- Each response must be a single JSON directive targeting one agent.
- The message field should be a natural language instruction for the targeted agent.
- Do NOT wrap messages in markdown code fences or add commentary outside the JSON.
- After each directive, you will see the agent's output prefixed with [agent_name].
- Analyze outputs carefully before deciding your next action.

## Queue behavior

- If an agent is busy processing a previous message, your message will be queued (max 1 per agent).
- You will be told "agent is busy, message queued" and should address a different agent.
- If an agent is busy AND already has a queued message, your message will be rejected.

## Workflow guidelines

A typical (but not required) workflow:
1. Use **planner** to analyze the task, understand requirements, and create a plan.
2. Use **executor** to implement the plan step by step.
3. Use **verifier** to run tests, lint, and validate the implementation.
4. Iterate between agents as needed based on their output.

## Memory

To save a fact for future sessions, include a line starting with "MEMORY_SAVE: " followed by the fact. These lines will be stripped before sending to agents.

## Completion

When the task is fully complete and you have verified the results, respond with exactly:
TASK_COMPLETE

Only send TASK_COMPLETE when you are confident the task is done. Send it as plain text, not inside JSON.`,
		agentName, agentName, agentName, agentName, agentName)

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

// parseAgentDirective extracts a JSON directive from the LLM reply.
// It first tries to parse the whole string, then extracts the first {...} block.
func parseAgentDirective(reply string) (AgentDirective, error) {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return AgentDirective{}, fmt.Errorf("parseAgentDirective: empty reply")
	}

	// Try whole string as JSON.
	var d AgentDirective
	if err := json.Unmarshal([]byte(reply), &d); err == nil {
		if d.Agent == "" || d.Message == "" {
			return AgentDirective{}, fmt.Errorf("parseAgentDirective: missing required fields (agent=%q, message=%q)", d.Agent, d.Message)
		}
		return d, nil
	}

	// Extract first {...} block.
	start := strings.IndexByte(reply, '{')
	if start < 0 {
		return AgentDirective{}, fmt.Errorf("parseAgentDirective: no JSON object found in reply")
	}
	// Find matching closing brace.
	depth := 0
	end := -1
	for i := start; i < len(reply); i++ {
		switch reply[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = i + 1
				break
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		return AgentDirective{}, fmt.Errorf("parseAgentDirective: unmatched braces in reply")
	}

	block := reply[start:end]
	if err := json.Unmarshal([]byte(block), &d); err != nil {
		return AgentDirective{}, fmt.Errorf("parseAgentDirective: invalid JSON: %w", err)
	}
	if d.Agent == "" || d.Message == "" {
		return AgentDirective{}, fmt.Errorf("parseAgentDirective: missing required fields (agent=%q, message=%q)", d.Agent, d.Message)
	}
	return d, nil
}

// findAgent looks up an agent by role name and returns a pointer to its state.
func findAgent(agents []AgentState, name string) (*AgentState, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("findAgent: empty agent name")
	}
	role := AgentRole(strings.ToLower(name))
	for i := range agents {
		if agents[i].Role == role {
			return &agents[i], nil
		}
	}
	return nil, fmt.Errorf("findAgent: unknown agent %q", name)
}

// sendToAgent dispatches a message to an agent's tmux session in a goroutine.
// It marks the agent as busy and sends the result on the provided channel.
// An overall deadline (SendToAgentTimeout) prevents goroutine leaks if tmux
// becomes unresponsive.
func sendToAgent(ag *AgentState, message, workDir, command string, ch chan<- agentResult) {
	ag.Status = statusBusy
	session := ag.Session
	role := ag.Role
	lastPane := ag.LastPane

	go func() {
		overallDeadline := time.Now().Add(SendToAgentTimeout)
		pane, err := tmux.SendAndCaptureWithRecovery(session, workDir, command, message, lastPane)

		// If the agent is still working (no changes or output still changing),
		// keep polling instead of reporting an error. Enforce an overall deadline
		// to prevent goroutine leaks if tmux becomes unresponsive.
		for err != nil && (errors.Is(err, tmux.ErrAgentStillWorking) || errors.Is(err, tmux.ErrDidNotStabilize)) {
			if time.Now().After(overallDeadline) {
				err = fmt.Errorf("sendToAgent: overall timeout (%s) exceeded while waiting for %s", SendToAgentTimeout, role)
				break
			}
			lastPane = pane
			pane, err = tmux.WaitForPaneUpdate(session, lastPane, 90*time.Second)
		}

		result := agentResult{
			Role: role,
			Pane: pane,
		}
		if err != nil {
			result.Err = err
		} else {
			result.Output = tmux.CleanPaneOutput(pane)
		}
		ch <- result
	}()
}

// allBusy returns true if all agents are currently busy.
func allBusy(agents []AgentState) bool {
	for _, ag := range agents {
		if ag.Status == statusIdle {
			return false
		}
	}
	return true
}

// summarizeContext compresses older messages when the conversation exceeds
// ContextSummaryThreshold. It keeps the system prompt and the last N recent
// messages, compressing the middle portion into a single summary via an LLM call.
// On failure it returns the original messages unchanged.
func summarizeContext(messages []Message, apiKey, model string) []Message {
	if len(messages) <= ContextSummaryThreshold {
		return messages
	}

	// Keep system prompt (index 0) and the most recent messages.
	keepRecent := ContextKeepRecent
	if keepRecent >= len(messages)-1 {
		return messages
	}

	toSummarize := messages[1 : len(messages)-keepRecent]
	recent := messages[len(messages)-keepRecent:]

	// Build summary prompt.
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation history into a concise summary. Preserve key decisions, results, errors, and current state. Return only the summary text, no JSON or formatting.\n\n")
	for _, msg := range toSummarize {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", msg.Role, msg.Content))
	}

	summaryMsgs := []Message{{Role: "user", Content: sb.String()}}
	summary, _, err := CallOpenRouter(apiKey, model, summaryMsgs, 0.2)
	if err != nil {
		fmt.Fprintf(os.Stderr, "│ Context summarization failed (non-fatal): %v\n", err)
		return messages
	}

	fmt.Printf("│ Summarized context: %d messages → 1 summary + %d recent\n", len(toSummarize), keepRecent)

	// Rebuild: system prompt + summary + recent messages.
	result := make([]Message, 0, 2+len(recent))
	result = append(result, messages[0]) // system prompt
	result = append(result, Message{Role: "user", Content: fmt.Sprintf("## Conversation summary (older context)\n%s", summary)})
	result = append(result, recent...)
	return result
}
