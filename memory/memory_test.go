package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LoadMemory returns nil, nil when the file does not exist.
func TestLoadMemory_MissingFile(t *testing.T) {
	dir := t.TempDir()
	facts, err := LoadMemory(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if facts != nil {
		t.Fatalf("expected nil, got %v", facts)
	}
}

// LoadMemory reads a valid memory.json file.
func TestLoadMemory_ValidFile(t *testing.T) {
	dir := t.TempDir()
	data := `["fact one", "fact two"]`
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte(data), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	facts, err := LoadMemory(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(facts) != 2 || facts[0] != "fact one" || facts[1] != "fact two" {
		t.Fatalf("unexpected facts: %v", facts)
	}
}

// LoadMemory returns an error for malformed JSON.
func TestLoadMemory_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.json"), []byte("not json"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadMemory(dir)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// SaveMemory writes facts to memory.json and they can be read back.
func TestSaveMemory_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	facts := []string{"fact A", "fact B"}
	if err := SaveMemory(dir, facts); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadMemory(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 || loaded[0] != "fact A" || loaded[1] != "fact B" {
		t.Fatalf("round-trip mismatch: %v", loaded)
	}
}

// SaveMemory with nil writes an empty array.
func TestSaveMemory_NilWritesEmptyArray(t *testing.T) {
	dir := t.TempDir()
	if err := SaveMemory(dir, nil); err != nil {
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

// ExtractMemorySaves extracts MEMORY_SAVE lines and returns cleaned reply.
func TestExtractMemorySaves_Basic(t *testing.T) {
	reply := "do something\nMEMORY_SAVE: project uses Go 1.23\nmore text\nMEMORY_SAVE: no external deps\nfinal line"
	facts, cleaned := ExtractMemorySaves(reply)
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

// ExtractMemorySaves returns no facts when none present.
func TestExtractMemorySaves_NoFacts(t *testing.T) {
	reply := "just a normal reply\nwith multiple lines"
	facts, cleaned := ExtractMemorySaves(reply)
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts, got %d", len(facts))
	}
	if cleaned != reply {
		t.Fatalf("cleaned should equal original: got %q", cleaned)
	}
}

// ExtractMemorySaves ignores empty MEMORY_SAVE lines.
func TestExtractMemorySaves_EmptyFact(t *testing.T) {
	reply := "text\nMEMORY_SAVE: \nmore"
	facts, _ := ExtractMemorySaves(reply)
	if len(facts) != 0 {
		t.Fatalf("expected 0 facts for empty value, got %d", len(facts))
	}
}

// DeduplicateMemory removes duplicates preserving order.
func TestDeduplicateMemory(t *testing.T) {
	facts := []string{"a", "b", "a", "c", "b", "d"}
	got := DeduplicateMemory(facts)
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

// CompactMemory parses a valid JSON array from the callback response.
func TestCompactMemory_Success(t *testing.T) {
	fn := func(prompt string) (string, error) {
		return `["consolidated fact 1", "consolidated fact 2"]`, nil
	}

	facts := []string{"fact 1", "fact 2", "fact 1 duplicate", "fact 3"}
	compacted, err := CompactMemory(fn, facts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(compacted) != 2 || compacted[0] != "consolidated fact 1" {
		t.Fatalf("unexpected compacted facts: %v", compacted)
	}
}

// CompactMemory returns original facts on callback error.
func TestCompactMemory_APIError(t *testing.T) {
	fn := func(prompt string) (string, error) {
		return "", fmt.Errorf("server error")
	}

	facts := []string{"fact A", "fact B"}
	got, err := CompactMemory(fn, facts)
	if err == nil {
		t.Fatal("expected error from CompactMemory")
	}
	if len(got) != 2 || got[0] != "fact A" {
		t.Fatalf("expected original facts on error, got: %v", got)
	}
}

// CompactMemory handles JSON wrapped in markdown code fences.
func TestCompactMemory_MarkdownFences(t *testing.T) {
	fn := func(prompt string) (string, error) {
		return "```json\n[\"fact\"]\n```", nil
	}

	got, err := CompactMemory(fn, []string{"old fact"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "fact" {
		t.Fatalf("unexpected result: %v", got)
	}
}
