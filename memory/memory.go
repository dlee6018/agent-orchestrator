package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the name of the persistent memory file.
const FileName = "memory.json"

// MaxFacts is the memory compaction threshold; overridden via MEMORY_MAX_FACTS.
var MaxFacts = 50

// CompactFunc is the callback signature for LLM-based memory compaction.
// It receives a prompt string and returns the LLM's reply.
type CompactFunc func(prompt string) (string, error)

// LoadMemory reads the memory file from workDir and returns the stored facts.
// If the file does not exist it returns nil, nil (SaveMemory creates the file).
func LoadMemory(workDir string) ([]string, error) {
	path := filepath.Join(workDir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("LoadMemory: %w", err)
	}
	var facts []string
	if err := json.Unmarshal(data, &facts); err != nil {
		return nil, fmt.Errorf("LoadMemory: unmarshal: %w", err)
	}
	return facts, nil
}

// SaveMemory writes the facts to the memory file in workDir.
func SaveMemory(workDir string, facts []string) error {
	if facts == nil {
		facts = []string{}
	}
	data, err := json.MarshalIndent(facts, "", "  ")
	if err != nil {
		return fmt.Errorf("SaveMemory: marshal: %w", err)
	}
	path := filepath.Join(workDir, FileName)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("SaveMemory: write: %w", err)
	}
	return nil
}

// ExtractMemorySaves scans the LLM reply for lines matching "MEMORY_SAVE: <text>",
// collects them as new facts, and returns the cleaned reply with those lines removed.
func ExtractMemorySaves(reply string) ([]string, string) {
	var facts []string
	var kept []string
	for _, line := range strings.Split(reply, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "MEMORY_SAVE:"); ok {
			fact := strings.TrimSpace(after)
			if fact != "" {
				facts = append(facts, fact)
			}
		} else {
			kept = append(kept, line)
		}
	}
	return facts, strings.Join(kept, "\n")
}

// DeduplicateMemory returns facts with duplicates removed, preserving order.
func DeduplicateMemory(facts []string) []string {
	seen := make(map[string]bool, len(facts))
	var out []string
	for _, f := range facts {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// CompactMemory asks the LLM (via the provided callback) to consolidate a list of facts into a shorter list.
// On failure it returns the original facts unchanged.
func CompactMemory(fn CompactFunc, facts []string) ([]string, error) {
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return facts, nil
	}
	prompt := fmt.Sprintf(`You are a memory compaction assistant. Below is a JSON array of facts from previous sessions. Consolidate them into a shorter list:
- Merge duplicate or near-duplicate entries
- Remove stale or irrelevant entries
- Combine related items into single entries
- Keep the most important and actionable facts

Return ONLY a valid JSON array of strings, nothing else.

Facts:
%s`, string(factsJSON))

	reply, err := fn(prompt)
	if err != nil {
		return facts, fmt.Errorf("CompactMemory: %w", err)
	}

	// Extract JSON array from the reply (handle possible markdown fences).
	cleaned := strings.TrimSpace(reply)
	if start := strings.Index(cleaned, "["); start >= 0 {
		if end := strings.LastIndex(cleaned, "]"); end > start {
			cleaned = cleaned[start : end+1]
		}
	}

	var compacted []string
	if err := json.Unmarshal([]byte(cleaned), &compacted); err != nil {
		return facts, fmt.Errorf("CompactMemory: parse response: %w", err)
	}
	if len(compacted) == 0 {
		return facts, nil
	}
	return compacted, nil
}
