package orchestrator

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dlee6018/agent-orchestrator/dashboard"
	"github.com/dlee6018/agent-orchestrator/memory"
	"github.com/dlee6018/agent-orchestrator/tmux"
)

// MaxIterations is the safety cap on agent loop iterations (0 means unlimited).
var MaxIterations = 0

// AutonomousLoop drives Claude Code via an LLM agent loop.
// It sends the task to the LLM, relays its decisions to Claude Code,
// and feeds back the pane output until the LLM signals TASK_COMPLETE.
// If MaxIterations > 0 the loop stops after that many iterations.
// memories carries persistent facts from previous sessions; new facts
// are extracted from MEMORY_SAVE: lines and saved on exit.
func AutonomousLoop(session, workDir, command, apiKey, model, task string, broker *dashboard.SSEBroker, memories []string) {
	fmt.Println("========================================")
	fmt.Println("AUTONOMOUS MODE")
	fmt.Printf("Model: %s\n", model)
	if MaxIterations > 0 {
		fmt.Printf("Max iterations: %d\n", MaxIterations)
	} else {
		fmt.Println("Max iterations: unlimited")
	}
	fmt.Printf("Task: %s\n", task)
	fmt.Println("========================================")

	broker.Publish(dashboard.IterationEvent{
		Type:      "task_info",
		Timestamp: time.Now().Format(time.RFC3339),
		MaxIter:   MaxIterations,
		Task:      task,
		Model:     model,
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
		{Role: "system", Content: BuildSystemPrompt(memories)},
		{Role: "user", Content: fmt.Sprintf("Task: %s\n\nYou are now connected to the Claude Code CLI. Send your first message to begin working on the task.", task)},
	}

	lastPane := ""
	consecutiveAPIErrors := 0

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
		})

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
				// Rebuild system prompt with compacted memories.
				messages[0] = Message{Role: "system", Content: BuildSystemPrompt(memories)}
			}
		}

		// Call the orchestrator LLM.
		reply, usage, err := CallOpenRouter(apiKey, model, messages, 0.3)
		if err != nil {
			consecutiveAPIErrors++
			fmt.Fprintf(os.Stderr, "│ API ERROR (%d/3): %v\n", consecutiveAPIErrors, err)
			broker.Publish(dashboard.IterationEvent{
				Type:      "error",
				Iteration: i,
				Timestamp: time.Now().Format(time.RFC3339),
				Error:     fmt.Sprintf("API error (%d/3): %v", consecutiveAPIErrors, err),
			})
			if consecutiveAPIErrors >= 3 {
				fmt.Fprintln(os.Stderr, "│ Too many consecutive API errors, aborting.")
				broker.Publish(dashboard.IterationEvent{
					Type:      "complete",
					Iteration: i,
					Timestamp: time.Now().Format(time.RFC3339),
					Error:     "aborted after 3 consecutive API errors",
				})
				return
			}
			fmt.Fprintln(os.Stderr, "│ Retrying in 5s...")
			time.Sleep(5 * time.Second)
			if MaxIterations > 0 {
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
		newFacts, cleanedReply := memory.ExtractMemorySaves(reply)
		if len(newFacts) > 0 {
			memories = append(memories, newFacts...)
			memories = memory.DeduplicateMemory(memories)
			fmt.Printf("│ Saved %d new memory fact(s) (total: %d)\n", len(newFacts), len(memories))
			reply = cleanedReply
		}

		// Check for task completion.
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
			})
			broker.Publish(dashboard.IterationEvent{
				Type:      "complete",
				Iteration: i,
				Timestamp: time.Now().Format(time.RFC3339),
				Task:      task,
			})
			return
		}

		// Send the LLM's reply to Claude Code.
		pane, err := tmux.SendAndCaptureWithRecovery(session, workDir, command, reply, lastPane)

		// If Claude Code is still working, keep polling instead of calling the LLM.
		for err != nil && strings.Contains(err.Error(), "claude code is still working") {
			fmt.Println("│ Claude Code is still working, waiting for output...")
			lastPane = pane
			pane, err = tmux.WaitForPaneUpdate(session, lastPane, 90*time.Second)
		}

		if err != nil {
			errMsg := fmt.Sprintf("Error sending to Claude Code: %v", err)
			fmt.Fprintf(os.Stderr, "│ TMUX ERROR: %v\n", err)
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
				Error:        fmt.Sprintf("tmux error: %v", err),
			})
			// Feed the error back so the LLM can adapt.
			messages = append(messages,
				Message{Role: "assistant", Content: reply},
				Message{Role: "user", Content: errMsg},
			)
			continue
		}

		cleaned := tmux.CleanPaneOutput(pane)

		// Log Claude Code's response.
		fmt.Println("│")
		fmt.Println("│ ╔══ CLAUDE CODE OUTPUT ══════════════════")
		for _, line := range strings.Split(cleaned, "\n") {
			fmt.Printf("│ ║ %s\n", line)
		}
		fmt.Println("│ ╚════════════════════════════════════════")
		fmt.Printf("└─────────────────────────────────────────\n")

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
			ClaudeOutput: cleaned,
		})

		// Append to conversation history.
		messages = append(messages,
			Message{Role: "assistant", Content: reply},
			Message{Role: "user", Content: fmt.Sprintf("Claude Code output:\n%s", cleaned)},
		)
		lastPane = pane
	}

	fmt.Fprintf(os.Stderr, "\nReached maximum iterations (%d) without task completion.\n", MaxIterations)
	broker.Publish(dashboard.IterationEvent{
		Type:      "complete",
		Iteration: MaxIterations,
		Timestamp: time.Now().Format(time.RFC3339),
		Error:     fmt.Sprintf("reached maximum iterations (%d) without task completion", MaxIterations),
	})
}
