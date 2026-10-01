package agent

import (
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/message"
)

// Fork patch (#1147): models occasionally hallucinate tool names ("gash",
// "brep", "read"). Fantasy answers with a bare "tool not found: X", and
// the turn is a paid step that produces nothing. Rewriting that one error
// to carry the closest real tool name (or the available list) turns the
// same step into a self-correction.

// toolNameSuggestion returns the closest available tool name for missing,
// or "" when nothing is close enough. "Close" means: case-insensitive
// prefix match within two characters of extra length, or edit distance
// of at most two (so "gash"→"bash", "brep"→"grep", but a wholly unrelated
// name gets no guess — a wrong guess costs more than a list).
func toolNameSuggestion(missing string, available []string) string {
	missing = strings.ToLower(missing)
	best := ""
	bestDist := -1
	for _, candidate := range available {
		lower := strings.ToLower(candidate)
		if strings.HasPrefix(lower, missing) || strings.HasPrefix(missing, lower) {
			if abs(len(lower)-len(missing)) <= 2 {
				return candidate
			}
		}
		dist := levenshtein(missing, lower)
		if dist <= 2 && (bestDist == -1 || dist < bestDist) {
			best = candidate
			bestDist = dist
		}
	}
	return best
}

// augmentUnknownToolResult rewrites fantasy's bare "tool not found: X"
// error to include the closest real tool name, or the list of available
// tools when nothing is close. Any other tool result passes through
// unchanged.
func (a *sessionAgent) augmentUnknownToolResult(result message.ToolResult) message.ToolResult {
	if !result.IsError {
		return result
	}
	missing, ok := strings.CutPrefix(result.Content, "tool not found: ")
	if !ok || missing == "" || strings.ContainsAny(missing, " \n") {
		return result
	}
	var available []string
	for _, tool := range a.tools.Copy() {
		available = append(available, tool.Info().Name)
	}
	if suggestion := toolNameSuggestion(missing, available); suggestion != "" {
		result.Content = fmt.Sprintf(
			"unknown tool %q — did you mean %q? (no tool with that name exists in this session)",
			missing, suggestion,
		)
		return result
	}
	result.Content = fmt.Sprintf(
		"unknown tool %q — no close match exists. Available tools: %s",
		missing, strings.Join(available, ", "),
	)
	return result
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
