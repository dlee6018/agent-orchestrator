package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// When no custom socket is set, args pass through unchanged.
func TestTmuxArgs_DefaultSocket(t *testing.T) {
	tmuxSocket = ""
	got := tmuxArgs("has-session", "-t", "abc")
	if len(got) != 3 {
		t.Fatalf("unexpected args length: got %d", len(got))
	}
	if got[0] != "has-session" || got[2] != "abc" {
		t.Fatalf("unexpected args: %#v", got)
	}
}

// A custom socket prepends "-L <socket>" to the args.
func TestTmuxArgs_CustomSocket(t *testing.T) {
	tmuxSocket = "mysock"
	got := tmuxArgs("capture-pane", "-p")
	if len(got) != 4 {
		t.Fatalf("unexpected args length: got %d", len(got))
	}
	if got[0] != "-L" || got[1] != "mysock" || got[2] != "capture-pane" {
		t.Fatalf("unexpected args: %#v", got)
	}
}

// Env var prefixes are preserved and the binary is resolved to an absolute path.
func TestResolveStartupCommand(t *testing.T) {
	got, err := resolveStartupCommand("GT_ROLE=refinery go version")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, " version") {
		t.Fatalf("unexpected command: %q", got)
	}
	if !strings.Contains(got, "/go") && !strings.Contains(got, "\\go") {
		t.Fatalf("expected absolute binary path in command, got %q", got)
	}
}

// KEY=VALUE tokens are detected; flags like --model=sonnet are not.
func TestIsShellEnvAssignment(t *testing.T) {
	if !isShellEnvAssignment("GT_ROLE=refinery") {
		t.Fatal("expected env assignment token")
	}
	if isShellEnvAssignment("--model=sonnet") {
		t.Fatal("flags should not be treated as env assignment")
	}
}

// Recovery is triggered for server/session loss but not for unrelated errors.
func TestShouldRecoverSession(t *testing.T) {
	if !shouldRecoverSession(errors.New("no server running on /tmp/tmux")) {
		t.Fatal("expected recovery for server loss")
	}
	if shouldRecoverSession(errors.New("permission denied opening file")) {
		t.Fatal("did not expect recovery for unrelated errors")
	}
}

// A dead pane line (dead=1) is parsed into dead=true, the exit status, and the command name.
func TestParsePaneStateLine_Dead(t *testing.T) {
	dead, status, current, err := parsePaneStateLine("1\t127\tclaude\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dead || status != 127 || current != "claude" {
		t.Fatalf("unexpected parse: dead=%v status=%d current=%q", dead, status, current)
	}
}

// A live pane line (dead=0) with an empty exit status parses as dead=false, status=0.
func TestParsePaneStateLine_Alive(t *testing.T) {
	dead, status, current, err := parsePaneStateLine("0\t\tbash\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dead || status != 0 || current != "bash" {
		t.Fatalf("unexpected parse: dead=%v status=%d current=%q", dead, status, current)
	}
}

// Malformed input returns a parse error.
func TestParsePaneStateLine_Invalid(t *testing.T) {
	_, _, _, err := parsePaneStateLine("bad-line")
	if err == nil {
		t.Fatal("expected parse error")
	}
}

// overrideTimers mutates package-level globals. Tests that call this
// must NOT use t.Parallel() — doing so would cause data races.
func overrideTimers(t *testing.T) {
	t.Helper()
	oldPoll := pollInterval
	oldStable := stableWindow
	pollInterval = 1 * time.Millisecond
	stableWindow = 3 * time.Millisecond
	t.Cleanup(func() {
		pollInterval = oldPoll
		stableWindow = oldStable
	})
}

// Once pane content changes and holds steady for the stable window, the new content is returned.
func TestWaitForPaneUpdateWithCapture_ReturnsStablePane(t *testing.T) {
	overrideTimers(t)

	panes := []string{"same", "new", "new", "new", "new"}
	i := 0
	capture := func() (string, error) {
		if i >= len(panes) {
			return panes[len(panes)-1], nil
		}
		v := panes[i]
		i++
		return v, nil
	}

	alwaysAlive := func() (bool, error) { return true, nil }
	got, err := waitForPaneUpdateWithCapture("same", 100*time.Millisecond, capture, alwaysAlive)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "new" {
		t.Fatalf("got %q, want new", got)
	}
}

// Times out with an error when the pane never changes from the previous value.
func TestWaitForPaneUpdateWithCapture_TimeoutNoChanges(t *testing.T) {
	overrideTimers(t)

	capture := func() (string, error) {
		return "same", nil
	}

	alwaysAlive := func() (bool, error) { return true, nil }
	got, err := waitForPaneUpdateWithCapture("same", 5*time.Millisecond, capture, alwaysAlive)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "still working") {
		t.Fatalf("expected 'still working' error, got: %v", err)
	}
	if got != "same" {
		t.Fatalf("got %q, want same", got)
	}
}

// Capture function errors propagate immediately without retrying.
func TestWaitForPaneUpdateWithCapture_CaptureError(t *testing.T) {
	capture := func() (string, error) {
		return "", errors.New("boom")
	}

	alwaysAlive := func() (bool, error) { return true, nil }
	_, err := waitForPaneUpdateWithCapture("same", 50*time.Millisecond, capture, alwaysAlive)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "boom" {
		t.Fatalf("unexpected error: %v", err)
	}
}

// When pane never changes and the process is dead, the error reports process death.
func TestWaitForPaneUpdateWithCapture_TimeoutProcessDead(t *testing.T) {
	overrideTimers(t)

	capture := func() (string, error) {
		return "same", nil
	}
	alwaysDead := func() (bool, error) { return false, nil }

	got, err := waitForPaneUpdateWithCapture("same", 5*time.Millisecond, capture, alwaysDead)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "process is dead") {
		t.Fatalf("expected 'process is dead' error, got: %v", err)
	}
	if got != "same" {
		t.Fatalf("got %q, want same", got)
	}
}

// ---------------------------------------------------------------------------
// Autonomous mode unit tests
// ---------------------------------------------------------------------------

// ANSI escape sequences and excessive blank lines are stripped.
func TestCleanPaneOutput_StripsANSI(t *testing.T) {
	input := "\x1b[32mHello\x1b[0m World\x1b[1m!\x1b[0m"
	got := cleanPaneOutput(input)
	if got != "Hello World!" {
		t.Fatalf("got %q, want %q", got, "Hello World!")
	}
}

// Runs of 3+ blank lines are collapsed to 2.
func TestCleanPaneOutput_CollapsesBlankLines(t *testing.T) {
	input := "line1\n\n\n\n\nline2"
	got := cleanPaneOutput(input)
	if !strings.Contains(got, "line1\n\nline2") {
		t.Fatalf("blank lines not collapsed: %q", got)
	}
}

// Trailing whitespace per line is removed.
func TestCleanPaneOutput_TrimsTrailingWhitespace(t *testing.T) {
	input := "hello   \nworld\t\t\n"
	got := cleanPaneOutput(input)
	lines := strings.Split(got, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t") != line {
			t.Fatalf("line %d has trailing whitespace: %q", i, line)
		}
	}
}

// Short strings pass through unchanged.
func TestTruncateForLog_Short(t *testing.T) {
	got := truncateForLog("hello", 10)
	if got != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}
}

// Long strings are truncated with "..." suffix.
func TestTruncateForLog_Long(t *testing.T) {
	got := truncateForLog("hello world", 8)
	if got != "hello..." {
		t.Fatalf("got %q, want %q", got, "hello...")
	}
}

// Very small maxLen doesn't panic.
func TestTruncateForLog_TinyMax(t *testing.T) {
	got := truncateForLog("hello", 2)
	if got != "he" {
		t.Fatalf("got %q, want %q", got, "he")
	}
}

// System prompt contains the completion marker instruction.
func TestBuildSystemPrompt_ContainsMarker(t *testing.T) {
	prompt := buildSystemPrompt(nil)
	if !strings.Contains(prompt, taskCompleteMarker) {
		t.Fatal("system prompt should reference TASK_COMPLETE marker")
	}
}

// System prompt includes memory facts when provided.
func TestBuildSystemPrompt_WithMemories(t *testing.T) {
	memories := []string{"Go 1.23 with no external deps", "Tests must not use t.Parallel()"}
	prompt := buildSystemPrompt(memories)
	if !strings.Contains(prompt, "Memory from previous sessions") {
		t.Fatal("prompt should include memory section header")
	}
	for _, fact := range memories {
		if !strings.Contains(prompt, fact) {
			t.Fatalf("prompt should include fact %q", fact)
		}
	}
}

// System prompt without memories has no memory section.
func TestBuildSystemPrompt_NoMemories(t *testing.T) {
	prompt := buildSystemPrompt(nil)
	if strings.Contains(prompt, "Memory from previous sessions") {
		t.Fatal("prompt should not include memory section when no memories")
	}
}

// System prompt includes MEMORY_SAVE instruction.
func TestBuildSystemPrompt_MemorySaveInstruction(t *testing.T) {
	prompt := buildSystemPrompt(nil)
	if !strings.Contains(prompt, "MEMORY_SAVE:") {
		t.Fatal("prompt should include MEMORY_SAVE instruction")
	}
}

// callOpenRouter parses a well-formed response from a mock server.
func TestCallOpenRouter_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request structure.
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		var req openRouterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("expected model test-model, got %s", req.Model)
		}

		resp := openRouterResponse{
			ID: "test-id",
			Choices: []openRouterChoice{
				{Message: openRouterMessage{Role: "assistant", Content: "test reply"}, FinishReason: "stop"},
			},
			Usage: openRouterUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{{Role: "user", Content: "hello"}}
	reply, usage, err := callOpenRouter("test-key", "test-model", msgs, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "test reply" {
		t.Fatalf("got reply %q, want %q", reply, "test reply")
	}
	if usage.TotalTokens != 15 {
		t.Fatalf("got total tokens %d, want 15", usage.TotalTokens)
	}
}

// callOpenRouter returns a descriptive error on API error responses.
func TestCallOpenRouter_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		resp := openRouterError{}
		resp.Error.Message = "rate limited"
		resp.Error.Code = 429
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{{Role: "user", Content: "hello"}}
	_, _, err := callOpenRouter("test-key", "test-model", msgs, 0.5)
	if err == nil {
		t.Fatal("expected error for 429 response")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error should mention rate limited: %v", err)
	}
}

// callOpenRouter returns an error when the response has no choices.
func TestCallOpenRouter_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openRouterResponse{ID: "test-id", Choices: []openRouterChoice{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{{Role: "user", Content: "hello"}}
	_, _, err := callOpenRouter("test-key", "test-model", msgs, 0.5)
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "empty choices") {
		t.Fatalf("error should mention empty choices: %v", err)
	}
}

// Verify request JSON structure sent to the API.
func TestCallOpenRouter_RequestStructure(t *testing.T) {
	var receivedReq openRouterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedReq)
		resp := openRouterResponse{
			ID:      "test-id",
			Choices: []openRouterChoice{{Message: openRouterMessage{Role: "assistant", Content: "ok"}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	msgs := []openRouterMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "usr"},
	}
	_, _, err := callOpenRouter("key", "mymodel", msgs, 0.7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedReq.Model != "mymodel" {
		t.Fatalf("model: got %q want %q", receivedReq.Model, "mymodel")
	}
	if len(receivedReq.Messages) != 2 {
		t.Fatalf("messages count: got %d want 2", len(receivedReq.Messages))
	}
	if fmt.Sprintf("%.1f", receivedReq.Temperature) != "0.7" {
		t.Fatalf("temperature: got %v want 0.7", receivedReq.Temperature)
	}
}

// ---------------------------------------------------------------------------
// Dashboard / SSE broker unit tests
// ---------------------------------------------------------------------------

// A single subscriber receives published events.
func TestSSEBroker_PublishSubscribe(t *testing.T) {
	b := newSSEBroker()
	ch, unsub := b.subscribe()
	defer unsub()

	event := iterationEvent{Type: "iteration_end", Iteration: 1}
	b.publish(event)

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
	b := newSSEBroker()
	ch1, unsub1 := b.subscribe()
	defer unsub1()
	ch2, unsub2 := b.subscribe()
	defer unsub2()

	b.publish(iterationEvent{Type: "task_info", Task: "hello"})

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
	b := newSSEBroker()

	// Publish task_info before any subscriber exists.
	b.publish(iterationEvent{Type: "task_info", Task: "my-task", Model: "test-model"})

	// A late subscriber should immediately receive the task_info replay.
	ch, unsub := b.subscribe()
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
	b := newSSEBroker()

	// Publish a non-task_info event.
	b.publish(iterationEvent{Type: "iteration_end", Iteration: 1})

	ch, unsub := b.subscribe()
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
	b := newSSEBroker()
	ch, unsub := b.subscribe()
	unsub()

	// Channel should be closed.
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after unsubscribe")
	}

	// Publishing after unsubscribe should not panic.
	b.publish(iterationEvent{Type: "complete"})
}

// A full buffer does not block the publisher.
func TestSSEBroker_SlowClientDrop(t *testing.T) {
	b := newSSEBroker()
	ch, unsub := b.subscribe()
	defer unsub()

	// Fill the buffer (capacity 64).
	for i := 0; i < 70; i++ {
		b.publish(iterationEvent{Type: "iteration_end", Iteration: i})
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

// emitEvent with a nil broker does not panic.
func TestEmitEvent_NilBroker(t *testing.T) {
	emitEvent(nil, iterationEvent{Type: "complete"})
}

// iterationEvent marshals to expected JSON.
func TestIterationEvent_JSON(t *testing.T) {
	event := iterationEvent{
		Type:         "iteration_end",
		Iteration:    3,
		MaxIter:      10,
		Timestamp:    "2026-01-01T00:00:00Z",
		DurationMs:   5000,
		Tokens:       &tokenUsage{Prompt: 100, Completion: 50, Total: 150},
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

// Dashboard serves static files from the embedded filesystem.
func TestStartDashboard_ServesStaticFiles(t *testing.T) {
	b := newSSEBroker()
	addr, err := startDashboard(b, 0)
	if err != nil {
		t.Fatalf("startDashboard: %v", err)
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
	b := newSSEBroker()
	addr, err := startDashboard(b, 0)
	if err != nil {
		t.Fatalf("startDashboard: %v", err)
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
	b.publish(iterationEvent{Type: "task_info", Task: "test-task"})

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

// ---------------------------------------------------------------------------
// Persistent memory unit tests
// ---------------------------------------------------------------------------

// loadMemory returns nil for a missing file.
func TestLoadMemory_MissingFile(t *testing.T) {
	dir := t.TempDir()
	facts, err := loadMemory(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts != nil {
		t.Fatalf("expected nil, got %v", facts)
	}
}

// loadMemory reads a valid memory.json file.
func TestLoadMemory_ValidFile(t *testing.T) {
	dir := t.TempDir()
	data := `["fact one", "fact two"]`
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte(data), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	facts, err := loadMemory(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 || facts[0] != "fact one" || facts[1] != "fact two" {
		t.Fatalf("unexpected facts: %v", facts)
	}
}

// loadMemory returns an error for malformed JSON.
func TestLoadMemory_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte("not json"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := loadMemory(dir)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// saveMemory writes facts to memory.json and they can be read back.
func TestSaveMemory_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	facts := []string{"fact A", "fact B"}
	if err := saveMemory(dir, facts); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := loadMemory(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 || loaded[0] != "fact A" || loaded[1] != "fact B" {
		t.Fatalf("round-trip mismatch: %v", loaded)
	}
}

// saveMemory with nil writes an empty array.
func TestSaveMemory_NilWritesEmptyArray(t *testing.T) {
	dir := t.TempDir()
	if err := saveMemory(dir, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("expected empty JSON array, got: %s", data)
	}
}

// extractMemorySaves extracts MEMORY_SAVE lines and returns cleaned reply.
func TestExtractMemorySaves_Basic(t *testing.T) {
	reply := "do something\nMEMORY_SAVE: project uses Go 1.23\nmore text\nMEMORY_SAVE: no external deps\nfinal line"
	facts, cleaned := extractMemorySaves(reply)
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d: %v", len(facts), facts)
	}
	if facts[0] != "project uses Go 1.23" || facts[1] != "no external deps" {
		t.Fatalf("unexpected facts: %v", facts)
	}
	if strings.Contains(cleaned, "MEMORY_SAVE") {
		t.Fatalf("cleaned reply should not contain MEMORY_SAVE: %q", cleaned)
	}
	if !strings.Contains(cleaned, "do something") || !strings.Contains(cleaned, "more text") || !strings.Contains(cleaned, "final line") {
		t.Fatalf("cleaned reply missing expected text: %q", cleaned)
	}
}

// extractMemorySaves returns no facts when none present.
func TestExtractMemorySaves_NoFacts(t *testing.T) {
	reply := "just a normal reply\nwith multiple lines"
	facts, cleaned := extractMemorySaves(reply)
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts, got %d", len(facts))
	}
	if cleaned != reply {
		t.Fatalf("cleaned should equal original: got %q", cleaned)
	}
}

// extractMemorySaves ignores empty MEMORY_SAVE lines.
func TestExtractMemorySaves_EmptyFact(t *testing.T) {
	reply := "text\nMEMORY_SAVE: \nmore"
	facts, _ := extractMemorySaves(reply)
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts for empty value, got %d", len(facts))
	}
}

// deduplicateMemory removes duplicates preserving order.
func TestDeduplicateMemory(t *testing.T) {
	facts := []string{"a", "b", "a", "c", "b", "d"}
	got := deduplicateMemory(facts)
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// compactMemory parses a valid JSON array from the LLM response.
func TestCompactMemory_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openRouterResponse{
			ID: "test-id",
			Choices: []openRouterChoice{
				{Message: openRouterMessage{Role: "assistant", Content: `["consolidated fact 1", "consolidated fact 2"]`}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	facts := []string{"fact 1", "fact 2", "fact 1 duplicate", "fact 3"}
	compacted, err := compactMemory("key", "model", facts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(compacted) != 2 || compacted[0] != "consolidated fact 1" {
		t.Fatalf("unexpected compacted facts: %v", compacted)
	}
}

// compactMemory returns original facts on API error.
func TestCompactMemory_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"server error"}}`))
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	facts := []string{"fact A", "fact B"}
	got, err := compactMemory("key", "model", facts)
	if err == nil {
		t.Fatal("expected error from compactMemory")
	}
	if len(got) != 2 || got[0] != "fact A" {
		t.Fatalf("expected original facts on error, got: %v", got)
	}
}

// compactMemory handles JSON wrapped in markdown code fences.
func TestCompactMemory_MarkdownFences(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openRouterResponse{
			ID: "test-id",
			Choices: []openRouterChoice{
				{Message: openRouterMessage{Role: "assistant", Content: "```json\n[\"fact\"]\n```"}},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	oldEndpoint := openRouterEndpoint
	openRouterEndpoint = srv.URL
	t.Cleanup(func() { openRouterEndpoint = oldEndpoint })

	got, err := compactMemory("key", "model", []string{"old fact"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "fact" {
		t.Fatalf("unexpected result: %v", got)
	}
}
