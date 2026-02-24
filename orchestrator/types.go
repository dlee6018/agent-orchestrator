package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dlee6018/agent-orchestrator/tmux"
)

// Endpoint is the OpenRouter API URL (var so tests can override).
var Endpoint = "https://openrouter.ai/api/v1/chat/completions"

// DefaultModel is the default OpenRouter model.
const DefaultModel = "anthropic/claude-opus-4.6"

// Provider is the preferred OpenRouter provider slug (e.g. "amazon-bedrock", "anthropic", "google-vertex").
// Empty string means let OpenRouter pick the best provider automatically.
var Provider = ""

// TaskCompleteMarker is the string the LLM sends to signal task completion.
const TaskCompleteMarker = "TASK_COMPLETE"

// Message represents a chat message in the OpenRouter API.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ProviderPreferences controls which provider OpenRouter routes the request to.
// The "only" field restricts routing to the specified providers exclusively.
type ProviderPreferences struct {
	Only []string `json:"only"`
}

// Request is the request body for the OpenRouter chat completion API.
type Request struct {
	Model       string               `json:"model"`
	Messages    []Message            `json:"messages"`
	Temperature float64              `json:"temperature"`
	Provider    *ProviderPreferences `json:"provider,omitempty"`
}

// Choice is a single completion choice from the OpenRouter API.
type Choice struct {
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage tracks token counts from the OpenRouter API response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Response is the response body from the OpenRouter chat completion API.
type Response struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// ErrorResponse is the error response body from the OpenRouter API.
type ErrorResponse struct {
	Error struct {
		Message  string         `json:"message"`
		Code     int            `json:"code"`
		Metadata *ErrorMetadata `json:"metadata,omitempty"`
	} `json:"error"`
}

// ErrorMetadata holds optional context from OpenRouter error responses.
type ErrorMetadata struct {
	ProviderName string      `json:"provider_name,omitempty"`
	Raw          interface{} `json:"raw,omitempty"`
}

// CallOpenRouter sends a chat completion request to the OpenRouter API
// and returns the assistant's reply content and token usage.
func CallOpenRouter(apiKey, model string, messages []Message, temperature float64) (string, Usage, error) {
	reqBody := Request{
		Model:       model,
		Messages:    messages,
		Temperature: temperature,
	}
	if Provider != "" {
		reqBody.Provider = &ProviderPreferences{Only: []string{Provider}}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", Usage{}, fmt.Errorf("CallOpenRouter: marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", Endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", Usage{}, fmt.Errorf("CallOpenRouter: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{
		Timeout: 300 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", Usage{}, fmt.Errorf("CallOpenRouter: HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", Usage{}, fmt.Errorf("CallOpenRouter: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr ErrorResponse
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
			msg := apiErr.Error.Message
			if apiErr.Error.Metadata != nil {
				if apiErr.Error.Metadata.ProviderName != "" {
					msg += fmt.Sprintf(" [provider: %s]", apiErr.Error.Metadata.ProviderName)
				}
				if apiErr.Error.Metadata.Raw != nil {
					rawJSON, _ := json.Marshal(apiErr.Error.Metadata.Raw)
					msg += fmt.Sprintf(" [raw: %s]", tmux.TruncateForLog(string(rawJSON), 500))
				}
			}
			return "", Usage{}, fmt.Errorf("CallOpenRouter: API error %d: %s", resp.StatusCode, msg)
		}
		return "", Usage{}, fmt.Errorf("CallOpenRouter: HTTP %d: %s", resp.StatusCode, tmux.TruncateForLog(string(body), 500))
	}

	var result Response
	if err := json.Unmarshal(body, &result); err != nil {
		return "", Usage{}, fmt.Errorf("CallOpenRouter: unmarshal response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", result.Usage, fmt.Errorf("CallOpenRouter: empty choices in response")
	}
	return result.Choices[0].Message.Content, result.Usage, nil
}

// AgentRole identifies a named agent in multi-agent mode.
type AgentRole string

const (
	AgentPlanner  AgentRole = "planner"
	AgentExecutor AgentRole = "executor"
	AgentVerifier AgentRole = "verifier"
)

// ValidAgentRoles is the set of recognized agent role names.
var ValidAgentRoles = map[AgentRole]bool{
	AgentPlanner: true, AgentExecutor: true, AgentVerifier: true,
}

type agentStatus int

const (
	statusIdle agentStatus = iota
	statusBusy             // message sent, goroutine polling for output
)

// AgentState holds per-agent runtime state.
type AgentState struct {
	Role      AgentRole
	Session   string // tmux session name
	LastPane  string // last captured pane content
	Status    agentStatus
	QueuedMsg string // pending message (max 1)
}

// AgentDirective is the parsed JSON from the orchestrator LLM.
type AgentDirective struct {
	Agent   string `json:"agent"`
	Message string `json:"message"`
}

// agentResult is sent by goroutines when an agent finishes processing.
type agentResult struct {
	Role   AgentRole
	Output string // cleaned pane output
	Pane   string // raw pane for lastPane tracking
	Err    error
}

// BuildSystemPrompt returns the system prompt for the orchestrator LLM.
// agentName is the display name of the inner coding agent (e.g. "Claude Code", "Codex").
func BuildSystemPrompt(agentName string, memories []string) string {
	base := fmt.Sprintf(`You are an orchestrator coordinating a %s CLI session via tmux.

## Your role

You are a HIGH-LEVEL COORDINATOR. %s is an AI-powered coding agent that understands natural language. Your job is to THINK about what needs to be done and give %s clear, high-level instructions in plain English. You are NOT the coding agent — do NOT type raw terminal commands, file paths, or shell operations.

## How to instruct %s

GOOD instructions (high-level, natural language):
- "Read the combat.js file and explain how the damage calculation works"
- "Create a new REST endpoint for user authentication using JWT tokens"
- "Run the test suite and fix any failing tests"
- "Refactor the database module to use connection pooling"
- "Find all files that handle payment processing and summarize the flow"

BAD instructions (raw commands — DO NOT do this):
- "cat ~/Desktop/project/src/combat.js"
- "grep -r 'function' src/"
- "vim main.go"
- "npm test && echo done"
- "ls -la src/components/"

%s will figure out which commands to run, which files to read, and how to accomplish the task. You just need to tell it WHAT to do, not HOW to do it at the terminal level.

## Rules

- Each response you give will be sent to the %s CLI as a natural language instruction.
- After each response, you will see %s's output showing what it did and its results.
- Analyze the output carefully before deciding your next instruction.
- If %s asks a question or needs confirmation, respond appropriately.
- If an approach fails, try a different strategy — do not repeat the same failed instruction.
- If %s shows an error, read it carefully and adapt your instructions.
- Keep your instructions concise and focused on the task.
- After each action, think about the next steps and proactively continue working. Do not wait passively.
- To save a fact for future sessions, include a line starting with "MEMORY_SAVE: " followed by the fact. These lines will be stripped before sending to %s. Use this to remember project conventions, pitfalls, user preferences, or anything useful across sessions.

When the task is fully complete and you have verified the results, respond with exactly:
TASK_COMPLETE

Only send TASK_COMPLETE when you are confident the task is done. Do not send it prematurely.`,
		agentName, agentName, agentName, agentName, agentName, agentName, agentName, agentName, agentName, agentName)

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
