package cliptool

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// BuiltinBridgeTools retains the legacy wire schemas and descriptions.
func BuiltinBridgeTools() ([]BridgeTool, error) {
	definitions := []struct {
		wire, canonical, description string
		input                        reflect.Type
	}{
		{"Bash", "bash", "Execute a shell command. Requires user approval.", reflect.TypeFor[MCPBashInput]()},
		{"Read", "view", "Read the contents of a file.", reflect.TypeFor[MCPViewInput]()},
		{"Write", "write", "Write content to a file, creating or overwriting it.", reflect.TypeFor[MCPWriteInput]()},
		{"Glob", "glob", "Find files matching a glob pattern.", reflect.TypeFor[MCPGlobInput]()},
		{"Grep", "grep", "Search file contents using a regular expression.", reflect.TypeFor[MCPGrepInput]()},
		{"todos", "todos", "Update the task list for the current session. Use this to create, update or complete tasks so the user can track progress.", reflect.TypeFor[MCPTodosInput]()},
	}
	result := make([]BridgeTool, 0, len(definitions))
	for _, definition := range definitions {
		schema, err := jsonschema.ForType(definition.input, &jsonschema.ForOptions{})
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			return nil, err
		}
		result = append(result, BridgeTool{WireName: definition.wire, CanonicalName: definition.canonical, Description: definition.description, InputSchema: raw})
	}
	return result, nil
}

func FilterBridgeRead(path, content string) string {
	if isClaudeMdPath(path) {
		return stripRushClaudeInitBlock(content)
	}
	return content
}

// rushClaudeInitBlockPattern is the same regex `internal/cmd/claude_init.go`
// uses to identify our injected block. Duplicated here to avoid a cmd→
// cliprovider import (cmd already imports a lot from the agent layer).
// If the marker scheme ever changes, update both sites.
var rushClaudeInitBlockPattern = regexp.MustCompile(`(?s)<!-- crush-claude-init:v\d+ -->.*?<!-- /crush-claude-init -->\s*`)

func isClaudeMdPath(path string) bool {
	// Split on BOTH separators regardless of host OS: the path may be a
	// Windows path (…\CLAUDE.md) even when rush runs on Linux/macOS, where
	// filepath.Base only understands "/".
	base := path
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	// Case-insensitive — Windows users sometimes write "Claude.md".
	return strings.EqualFold(base, "CLAUDE.md")
}

func stripRushClaudeInitBlock(content string) string {
	return rushClaudeInitBlockPattern.ReplaceAllString(content, "")
}

type MCPBashInput struct {
	Command     string `json:"command"     description:"Shell command to execute"`
	Description string `json:"description" description:"Brief description of what the command does"`
	WorkingDir  string `json:"working_dir,omitempty" description:"Working directory (defaults to project root)"`
}

type MCPViewInput struct {
	Path      string `json:"path"                description:"File path to read"`
	StartLine int    `json:"start_line,omitempty" description:"First line to read (1-based, 0 = beginning)"`
	EndLine   int    `json:"end_line,omitempty"   description:"Last line to read (0 = end of file)"`
}

type MCPWriteInput struct {
	Path    string `json:"path"    description:"File path to write"`
	Content string `json:"content" description:"Content to write to the file"`
}

type MCPGlobInput struct {
	Pattern string `json:"pattern" description:"Glob pattern (e.g. **/*.go)"`
	Path    string `json:"path,omitempty" description:"Directory to search in"`
}

type MCPGrepInput struct {
	Pattern string `json:"pattern" description:"Regular expression to search for"`
	Path    string `json:"path,omitempty" description:"Directory or file to search in"`
	Glob    string `json:"glob,omitempty" description:"File glob filter (e.g. *.go)"`
}

type MCPTodosInput struct {
	Todos []MCPTodoItem `json:"todos" description:"The updated todo list"`
}

type MCPTodoItem struct {
	Content    string `json:"content"     description:"What needs to be done (imperative form)"`
	Status     string `json:"status"      description:"Task status: pending, in_progress, or completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g. 'Running tests')"`
}

// RunAllowlistCommand lets the restricted-run gate apply allow_bash
// pattern scrutiny to this tool's command string exactly as it does for
// the built-in bash tool (see internal/permission/runallowlist.go).
func (p MCPBashInput) RunAllowlistCommand() string { return p.Command }
