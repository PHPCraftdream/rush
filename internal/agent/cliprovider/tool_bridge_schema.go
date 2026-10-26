package cliprovider

import (
	"encoding/json"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// BuiltinBridgeTools retains the legacy wire schemas and descriptions.
func BuiltinBridgeTools() ([]BridgeTool, error) {
	definitions := []struct {
		wire, canonical, description string
		input                        reflect.Type
	}{
		{"Bash", "bash", "Execute a shell command. Requires user approval.", reflect.TypeFor[mcpBashInput]()},
		{"Read", "view", "Read the contents of a file.", reflect.TypeFor[mcpViewInput]()},
		{"Write", "write", "Write content to a file, creating or overwriting it.", reflect.TypeFor[mcpWriteInput]()},
		{"Glob", "glob", "Find files matching a glob pattern.", reflect.TypeFor[mcpGlobInput]()},
		{"Grep", "grep", "Search file contents using a regular expression.", reflect.TypeFor[mcpGrepInput]()},
		{"todos", "todos", "Update the task list for the current session. Use this to create, update or complete tasks so the user can track progress.", reflect.TypeFor[mcpTodosInput]()},
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
