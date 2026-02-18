package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dlee6018/agent-orchestrator/tmux"
)

// Endpoint is the OpenRouter API URL (var so tests can override).
var Endpoint = "https://openrouter.ai/api/v1/chat/completions"

// DefaultModel is the default OpenRouter model.
const DefaultModel = "anthropic/claude-opus-4.6"

// TaskCompleteMarker is the string the LLM sends to signal task completion.
const TaskCompleteMarker = "TASK_COMPLETE"

// Message represents a chat message in the OpenRouter API.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the request body for the OpenRouter chat completion API.
type Request struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
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
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// CallOpenRouter sends a chat completion request to the OpenRouter API
// and returns the assistant's reply content and token usage.
func CallOpenRouter(apiKey, model string, messages []Message, temperature float64) (string, Usage, error) {
	reqBody := Request{
		Model:       model,
		Messages:    messages,
		Temperature: temperature,
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

	client := &http.Client{Timeout: 120 * time.Second}
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
			return "", Usage{}, fmt.Errorf("CallOpenRouter: API error %d: %s", resp.StatusCode, apiErr.Error.Message)
		}
		return "", Usage{}, fmt.Errorf("CallOpenRouter: HTTP %d: %s", resp.StatusCode, tmux.TruncateForLog(string(body), 200))
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

// BuildSystemPrompt returns the system prompt for the orchestrator LLM.
func BuildSystemPrompt(memories []string) string {
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
