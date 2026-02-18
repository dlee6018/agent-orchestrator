package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os/exec"
	"sync"
)

//go:embed web/*
var webContent embed.FS

// Dashboard types.

// IterationEvent represents an SSE event payload for the web dashboard.
type IterationEvent struct {
	Type         string      `json:"type"` // "task_info", "iteration_start", "iteration_end", "error", "complete"
	Iteration    int         `json:"iteration"`
	MaxIter      int         `json:"max_iter"`
	Timestamp    string      `json:"timestamp"`
	DurationMs   int64       `json:"duration_ms,omitempty"`
	Tokens       *TokenUsage `json:"tokens,omitempty"`
	Orchestrator string      `json:"orchestrator,omitempty"`
	ClaudeOutput string      `json:"claude_output,omitempty"`
	Error        string      `json:"error,omitempty"`
	Task         string      `json:"task,omitempty"`
	Model        string      `json:"model,omitempty"`
}

// TokenUsage tracks prompt, completion, and total token counts.
type TokenUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// SSEBroker manages fan-out of SSE events to multiple connected clients.
// It retains the last task_info payload so late-connecting clients (or
// reconnects) immediately receive the current task metadata.
type SSEBroker struct {
	mu           sync.Mutex
	clients      []chan string
	lastTaskInfo string // SSE payload for the most recent task_info event
}

// NewSSEBroker creates a new SSEBroker instance.
func NewSSEBroker() *SSEBroker {
	return &SSEBroker{}
}

// Subscribe adds a new client and returns its event channel and an unsubscribe function.
// If a task_info event was previously published, it is replayed to the new client immediately.
func (b *SSEBroker) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 64) // buffer channel of 64
	b.mu.Lock()
	b.clients = append(b.clients, ch)
	// Replay the last task_info so late joiners see the task metadata.
	if b.lastTaskInfo != "" {
		select {
		case ch <- b.lastTaskInfo: // non-blocking, may drop if not available
		default:
		}
	}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, c := range b.clients {
			if c == ch {
				b.clients = append(b.clients[:i], b.clients[i+1:]...) // skip the ith
				close(ch)
				return
			}
		}
	}
}

// Publish sends an event to all connected clients (non-blocking).
// task_info events are retained so they can be replayed to late subscribers.
// Safe to call on a nil receiver (no-op).
func (b *SSEBroker) Publish(event IterationEvent) {
	if b == nil {
		return
	}
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

// StartDashboard starts the web dashboard HTTP server.
// It returns the address the server is listening on.
func StartDashboard(broker *SSEBroker, port int) (string, error) {
	mux := http.NewServeMux()

	webFS, err := fs.Sub(webContent, "web")
	if err != nil {
		return "", fmt.Errorf("StartDashboard: fs.Sub: %w", err)
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

		ch, unsubscribe := broker.Subscribe()
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
		return "", fmt.Errorf("StartDashboard: listen: %w", err)
	}

	actualAddr := listener.Addr().String()
	server := &http.Server{Handler: mux}
	go server.Serve(listener)

	return actualAddr, nil
}

// OpenBrowser attempts to open the URL in the default browser.
func OpenBrowser(url string) {
	_ = exec.Command("open", url).Start()
}
