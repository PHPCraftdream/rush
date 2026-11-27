package agent

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	mcpoutput "github.com/PHPCraftdream/rush/internal/session/state/mcpoutput"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Activation requires the finalization-aware session service adapter. The base
// service intentionally has no MCPOutputStore method: nil means legacy output,
// never truncation without durable retention or an unrestricted write fallback.
func (c *coordinator) mcpOutputAccess() *tools.MCPOutputAccess {
	access := &tools.MCPOutputAccess{
		Allowed: make(map[string]bool), Permissions: c.permissions, WorkingDir: c.cfg.WorkingDir(),
	}
	if provider, ok := c.sessions.(interface {
		MCPOutputStore() *mcpoutput.MCPOutputStore
	}); ok {
		if store := provider.MCPOutputStore(); store != nil {
			access.Store = store
		}
	}
	return access
}

func admitMCPOutputSources(access *tools.MCPOutputAccess, final []fantasy.AgentTool) {
	for _, tool := range final {
		if source, ok := tool.(*tools.Tool); ok {
			access.Allowed[source.Name()] = true
		}
	}
}

//go:embed templates/coder.md.tpl
var coderPromptTmpl []byte

//go:embed templates/task.md.tpl
var taskPromptTmpl []byte

//go:embed templates/initialize.md.tpl
var initializePromptTmpl []byte

func coderPrompt(opts ...prompt.Option) (*prompt.Prompt, error) {
	systemPrompt, err := prompt.NewPrompt("coder", string(coderPromptTmpl), opts...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func taskPrompt(opts ...prompt.Option) (*prompt.Prompt, error) {
	systemPrompt, err := prompt.NewPrompt("task", string(taskPromptTmpl), opts...)
	if err != nil {
		return nil, err
	}
	return systemPrompt, nil
}

func InitializePrompt(cfg *config.ConfigStore) (string, error) {
	systemPrompt, err := prompt.NewPrompt("initialize", string(initializePromptTmpl))
	if err != nil {
		return "", err
	}
	// One-shot, not part of a turn: nothing was resolved against an earlier
	// generation, so the current config IS the pinned config. Passed
	// explicitly all the same -- Build no longer fetches it.
	return systemPrompt.Build(context.Background(), "", "", cfg, cfg.Config(), false)
}

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
			"tool not found: %q — did you mean %q? (no tool with that name exists in this session)",
			missing, suggestion,
		)
		return result
	}
	result.Content = fmt.Sprintf(
		"tool not found: %q — no close match exists. Available tools: %s",
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
