package clibridge

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

type invokeFunc interface {
	~func(context.Context, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error)
}

func NewCLIToolBridge[R, B any, I invokeFunc](ctor func(context.Context, R, I, []cliprovider.BridgeTool) (B, error), registry R, invoke I, external []cliprovider.BridgeTool) (B, error) {
	return ctor(context.Background(), registry, invoke, external)
}

func TranslateCLIInput(wire, input string) (string, string, error) {
	var params map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", "", err
	}
	if params == nil {
		return "", "", fmt.Errorf("CLI input must be an object")
	}
	path := ""
	switch wire {
	case "Read":
		var read struct {
			Path  string `json:"path"`
			Start int    `json:"start_line"`
			End   int    `json:"end_line"`
		}
		if err := json.Unmarshal([]byte(input), &read); err != nil {
			return "", "", err
		}
		if read.Start < 0 || read.End < 0 {
			return "", "", fmt.Errorf("read range cannot be negative")
		}
		start := max(1, read.Start)
		if read.End != 0 && read.End < start {
			return "", "", fmt.Errorf("read end precedes start")
		}
		limit := tools.DefaultReadLimit
		if read.End != 0 {
			limit = read.End - start + 1
		}
		raw, err := json.Marshal(tools.ViewParams{FilePath: read.Path, Offset: start - 1, Limit: limit})
		return string(raw), read.Path, err
	case "Write":
		params["file_path"] = params["path"]
		delete(params, "path")
	case "Grep":
		if glob, ok := params["glob"]; ok {
			params["include"] = glob
			delete(params, "glob")
		}
	case "Bash":
		for _, forbidden := range []string{"timeout", "run_in_background", "auto_background_after", "background"} {
			if _, ok := params[forbidden]; ok {
				return "", "", fmt.Errorf("CLI Bash does not accept %s", forbidden)
			}
		}
	}
	if wire == "" {
		return input, path, nil
	}
	raw, err := json.Marshal(params)
	return string(raw), path, err
}
