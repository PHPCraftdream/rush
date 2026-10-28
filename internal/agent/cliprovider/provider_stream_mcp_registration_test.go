package cliprovider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type mcpRegistrationTestBridge struct{}

func (mcpRegistrationTestBridge) Tools() []BridgeTool {
	return []BridgeTool{{WireName: "probe", CanonicalName: "probe", Description: "probe", InputSchema: json.RawMessage(`{"type":"object"}`)}}
}

func (mcpRegistrationTestBridge) Call(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.ToolResponse{Type: "text"}, nil
}

func TestStartStreamQwenSameProjectSessionRegistrations(t *testing.T) {
	isolateHomeDir(t)
	runMCPStartStreamRegistrationTest(t, true)
}

func TestStartStreamGeminiSameProjectSessionRegistrations(t *testing.T) {
	isolateHomeDir(t)
	runMCPStartStreamRegistrationTest(t, false)
}

func runMCPStartStreamRegistrationTest(t *testing.T, qwen bool) {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	workingDir := filepath.Join(t.TempDir(), "project")
	require.NoError(t, os.MkdirAll(filepath.Join(workingDir, ".rush"), 0o755))
	executable, err := os.Executable()
	require.NoError(t, err)
	integration := CLISpec{Binary: executable, NoPTY: true, BuildArgs: func(bool) []string { return nil }}
	if qwen {
		integration.QwenMCPIntegration = true
	} else {
		integration.GeminiMCPIntegration = true
	}
	type running struct {
		name   string
		args   []string
		finish func()
	}
	start := func(sessionID string) running {
		var args []string
		model := &cliModel{
			spec: integration, workingDir: workingDir,
			streamProcess: func(_ bool, got []string) procHandle {
				args = append([]string(nil), got...)
				return procHandle{stdout: strings.NewReader(""), kill: func() {}, wait: func() (string, error) { return "", nil }}
			},
		}
		// Supply the process seam without resolving or launching a real CLI.
		ctx := WithToolBridge(context.WithValue(t.Context(), SessionIDContextKey, sessionID), mcpRegistrationTestBridge{})
		stream, startErr := model.StartStream(ctx, fantasy.Call{})
		require.NoError(t, startErr)
		var once sync.Once
		finish := func() {
			once.Do(func() { stream.Stream()(func(fantasy.StreamPart) bool { return true }) })
		}
		t.Cleanup(finish)
		name := allowedMCPName(t, args)
		return running{name: name, args: args, finish: finish}
	}
	first := start("session-a")
	second := start("session-b")
	require.NotEqual(t, first.name, second.name)
	readURL := readQwenMCPServerURL
	if !qwen {
		readURL = readGeminiMCPServerURL
	}
	urlA, ok := readURL(t, home, first.name)
	require.True(t, ok)
	urlB, ok := readURL(t, home, second.name)
	require.True(t, ok)
	require.NotEqual(t, urlA, urlB)
	providerDir := ".qwen"
	if !qwen {
		providerDir = ".gemini"
	}
	data, err := os.ReadFile(filepath.Join(home, providerDir, "settings.json"))
	require.NoError(t, err)
	var settings struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	require.NoError(t, json.Unmarshal(data, &settings))
	require.Contains(t, settings.MCPServers, first.name)
	require.Contains(t, settings.MCPServers, second.name)
	require.True(t, strings.HasPrefix(readMCPAuthorization(t, home, providerDir, first.name), "Bearer "))
	require.True(t, strings.HasPrefix(readMCPAuthorization(t, home, providerDir, second.name), "Bearer "))
	require.Equal(t, first.name, argsFlagValue(t, first.args, "--allowed-mcp-server-names"))
	require.Equal(t, second.name, argsFlagValue(t, second.args, "--allowed-mcp-server-names"))
	if qwen {
		require.Contains(t, first.args, "mcp__"+first.name+"__probe")
		require.NotContains(t, first.args, "mcp__"+second.name+"__probe")
	}
	first.finish()
	_, ok = readURL(t, home, first.name)
	require.False(t, ok, "stream cleanup must deregister its own server")
	_, ok = readURL(t, home, second.name)
	require.True(t, ok, "stream cleanup must preserve the other session")
	second.finish()
}

func argsFlagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	t.Fatalf("flag %q missing from args: %q", flag, args)
	return ""
}

func allowedMCPName(t *testing.T, args []string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--allowed-mcp-server-names" {
			return args[i+1]
		}
	}
	t.Fatalf("allowed MCP server name missing from args: %q", args)
	return ""
}
