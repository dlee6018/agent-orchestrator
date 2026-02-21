package dashboard

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A single subscriber receives published events.
func TestSSEBroker_PublishSubscribe(t *testing.T) {
	b := NewSSEBroker()
	ch, unsub := b.Subscribe()
	defer unsub()

	event := IterationEvent{Type: "iteration_end", Iteration: 1}
	b.Publish(event)

	select {
	case msg := <-ch:
		if !strings.Contains(msg, `"type":"iteration_end"`) {
			t.Fatalf("unexpected message: %s", msg)
		}
		if !strings.Contains(msg, `"iteration":1`) {
			t.Fatalf("unexpected message: %s", msg)
		}
		if !strings.HasPrefix(msg, "data: ") {
			t.Fatalf("expected SSE data prefix, got: %s", msg)
		}
	default:
		t.Fatal("expected a message on the channel")
	}
}

// Multiple subscribers each receive the same event.
func TestSSEBroker_MultipleClients(t *testing.T) {
	b := NewSSEBroker()
	ch1, unsub1 := b.Subscribe()
	defer unsub1()
	ch2, unsub2 := b.Subscribe()
	defer unsub2()

	b.Publish(IterationEvent{Type: "task_info", Task: "hello"})

	for _, ch := range []<-chan string{ch1, ch2} {
		select {
		case msg := <-ch:
			if !strings.Contains(msg, `"task":"hello"`) {
				t.Fatalf("unexpected message: %s", msg)
			}
		default:
			t.Fatal("expected a message on all channels")
		}
	}
}

// Late subscribers receive the last task_info event on connect.
func TestSSEBroker_ReplayTaskInfo(t *testing.T) {
	b := NewSSEBroker()

	// Publish task_info before any subscriber exists.
	b.Publish(IterationEvent{Type: "task_info", Task: "my-task", Model: "test-model"})

	// A late subscriber should immediately receive the task_info replay.
	ch, unsub := b.Subscribe()
	defer unsub()

	select {
	case msg := <-ch:
		if !strings.Contains(msg, `"task":"my-task"`) {
			t.Fatalf("replayed message missing task: %s", msg)
		}
		if !strings.Contains(msg, `"type":"task_info"`) {
			t.Fatalf("replayed message missing type: %s", msg)
		}
	default:
		t.Fatal("expected task_info replay on subscribe, got nothing")
	}
}

// Late subscribers do NOT receive replay for non-task_info events.
func TestSSEBroker_NoReplayForOtherEvents(t *testing.T) {
	b := NewSSEBroker()

	// Publish a non-task_info event.
	b.Publish(IterationEvent{Type: "iteration_end", Iteration: 1})

	ch, unsub := b.Subscribe()
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("should not replay non-task_info events, got: %s", msg)
	default:
		// Expected: no replay.
	}
}

// After unsubscribing, the channel is closed and no further events arrive.
func TestSSEBroker_Unsubscribe(t *testing.T) {
	b := NewSSEBroker()
	ch, unsub := b.Subscribe()
	unsub()

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}

	// Publishing after unsubscribe should not panic.
	b.Publish(IterationEvent{Type: "complete"})
}

// A full buffer does not block the publisher.
func TestSSEBroker_SlowClientDrop(t *testing.T) {
	b := NewSSEBroker()
	ch, unsub := b.Subscribe()
	defer unsub()

	// Fill the buffer (capacity 64).
	for i := 0; i < 70; i++ {
		b.Publish(IterationEvent{Type: "iteration_end", Iteration: i})
	}

	// Should have 64 messages (buffer size), rest dropped.
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	if count != 64 {
		t.Fatalf("expected 64 buffered messages, got %d", count)
	}
}

// Publish on a nil broker does not panic.
func TestPublish_NilBroker(t *testing.T) {
	var b *SSEBroker
	b.Publish(IterationEvent{Type: "complete"})
}

// IterationEvent marshals to expected JSON.
func TestIterationEvent_JSON(t *testing.T) {
	event := IterationEvent{
		Type:         "iteration_end",
		Iteration:    3,
		MaxIter:      10,
		Timestamp:    "2026-01-01T00:00:00Z",
		DurationMs:   5000,
		Tokens:       &TokenUsage{Prompt: 100, Completion: 50, Total: 150},
		Orchestrator: "echo hello",
		ClaudeOutput: "hello\n",
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"type":"iteration_end"`, `"iteration":3`, `"max_iter":10`, `"duration_ms":5000`, `"prompt":100`, `"total":150`} {
		if !strings.Contains(s, want) {
			t.Fatalf("JSON missing %q: %s", want, s)
		}
	}
	// omitempty: error should not appear.
	if strings.Contains(s, `"error"`) {
		t.Fatalf("empty error should be omitted: %s", s)
	}
}

// IterationEvent includes both agent_output and claude_output in JSON when set.
func TestIterationEvent_AgentOutputJSON(t *testing.T) {
	event := IterationEvent{
		Type:         "iteration_end",
		Iteration:    1,
		ClaudeOutput: "hello\n",
		AgentOutput:  "hello\n",
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"agent_output"`) {
		t.Fatalf("JSON missing agent_output: %s", s)
	}
	if !strings.Contains(s, `"claude_output"`) {
		t.Fatalf("JSON missing claude_output: %s", s)
	}
}

// Dashboard serves static files from the embedded filesystem.
func TestStartDashboard_ServesStaticFiles(t *testing.T) {
	b := NewSSEBroker()
	addr, err := StartDashboard(b, 0)
	if err != nil {
		t.Fatalf("StartDashboard: %v", err)
	}
	base := "http://" + addr

	// Check index.html
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Orchestrator Dashboard") {
		t.Fatal("index.html does not contain expected title")
	}

	// Check style.css
	resp2, err := http.Get(base + "/style.css")
	if err != nil {
		t.Fatalf("GET /style.css: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("GET /style.css: status %d", resp2.StatusCode)
	}

	// Check app.js
	resp3, err := http.Get(base + "/app.js")
	if err != nil {
		t.Fatalf("GET /app.js: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("GET /app.js: status %d", resp3.StatusCode)
	}
}

// Dashboard SSE endpoint streams events.
func TestStartDashboard_SSEStream(t *testing.T) {
	b := NewSSEBroker()
	addr, err := StartDashboard(b, 0)
	if err != nil {
		t.Fatalf("StartDashboard: %v", err)
	}

	resp, err := http.Get("http://" + addr + "/events")
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected content-type: %s", resp.Header.Get("Content-Type"))
	}

	// Read the initial "connected" event.
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if !strings.Contains(line, "connected") {
		t.Fatalf("expected connected event, got: %s", line)
	}

	// Publish an event and verify it arrives.
	b.Publish(IterationEvent{Type: "task_info", Task: "test-task"})

	// Skip empty lines from previous event, then read next data line.
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimSpace(line)
		if line != "" {
			break
		}
	}
	if !strings.Contains(line, "test-task") {
		t.Fatalf("expected task_info event with test-task, got: %s", line)
	}
}
