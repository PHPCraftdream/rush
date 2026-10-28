package cliprovider

// Session-scoped IDs (see sessionMCPName) now isolate concurrent sessions; the
// expected-URL check additionally prevents cleanup from deleting a replacement
// registration under the same name.
//
// This test isolates os.UserHomeDir() (not the repo's own cached
// internal/home.Dir() — qwenSettingsPath/geminiSettingsPath call the
// stdlib directly, which re-reads HOME/USERPROFILE on every call, so
// t.Setenv works here without the package-init-time-caching pitfall found
// elsewhere in this codebase).
//
// REVERT CHECK PROCEDURE:
//  1. In provider_stream.go, replace sessionMCPName(id, sessionID) with id at
//     both Qwen and Gemini registration call sites.
//  2. Run: go test ./internal/agent/cliprovider/ -run 'TestStartStream(Qwen|Gemini)SameProjectSessionRegistrations' -count=1 -p 1
//  3. FAIL: both tests observe same-project sessions sharing the allowed name.
//  4. Restore sessionMCPName at both call sites and PASS.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func isolateHomeDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)        // POSIX
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOMEDRIVE", "")     // avoid HOMEDRIVE+HOMEPATH overriding USERPROFILE on some Windows Go versions
	t.Setenv("HOMEPATH", "")
	return home
}

func readQwenMCPServerURL(t *testing.T, home, serverName string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".qwen", "settings.json"))
	if os.IsNotExist(err) {
		return "", false
	}
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return "", false
	}
	entry, ok := mcpServers[serverName].(map[string]any)
	if !ok {
		return "", false
	}
	url, _ := entry["httpUrl"].(string)
	return url, true
}

func readMCPAuthorization(t *testing.T, home, providerDir, serverName string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, providerDir, "settings.json"))
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	servers := settings["mcpServers"].(map[string]any)
	entry := servers[serverName].(map[string]any)
	headers := entry["headers"].(map[string]any)
	return headers["Authorization"].(string)
}

func TestQwenMCP_SameProjectSessionsHaveIsolatedRegistrations(t *testing.T) {
	home := isolateHomeDir(t)
	workingDir := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(workingDir, ".rush"), 0o755))

	projectID, err := qwenMCPID(workingDir)
	require.NoError(t, err)
	const sessionA, sessionB = "session-a", "session-b"
	sessionAName := sessionMCPName(projectID, sessionA)
	sessionBName := sessionMCPName(projectID, sessionB)
	require.NotEqual(t, sessionAName, sessionBName)
	require.NoError(t, registerQwenMCP(sessionAName, "127.0.0.1:5001", "token-a"))
	require.NoError(t, registerQwenMCP(sessionBName, "127.0.0.1:5002", "token-b"))

	url, ok := readQwenMCPServerURL(t, home, sessionAName)
	require.True(t, ok, "session B registration must not replace session A")
	require.Equal(t, "http://127.0.0.1:5001/mcp", url)
	require.Equal(t, "Bearer token-a", readMCPAuthorization(t, home, ".qwen", sessionAName))
	url, ok = readQwenMCPServerURL(t, home, sessionBName)
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)
	require.Equal(t, "Bearer token-b", readMCPAuthorization(t, home, ".qwen", sessionBName))

	deregisterQwenMCP(sessionAName, "127.0.0.1:5001")
	_, ok = readQwenMCPServerURL(t, home, sessionAName)
	require.False(t, ok, "session A cleanup must remove only A")
	url, ok = readQwenMCPServerURL(t, home, sessionBName)
	require.True(t, ok, "session A cleanup must preserve B")
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)
	deregisterQwenMCP(sessionBName, "127.0.0.1:5002")
}

func TestGeminiMCP_SameProjectSessionsHaveIsolatedRegistrations(t *testing.T) {
	home := isolateHomeDir(t)
	workingDir := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(workingDir, ".rush"), 0o755))

	projectID, err := geminiMCPID(workingDir)
	require.NoError(t, err)
	const sessionA, sessionB = "session-a", "session-b"
	sessionAName := sessionMCPName(projectID, sessionA)
	sessionBName := sessionMCPName(projectID, sessionB)
	require.NotEqual(t, sessionAName, sessionBName)

	require.NoError(t, registerGeminiMCP(sessionAName, "127.0.0.1:5001", "token-a"))
	require.NoError(t, registerGeminiMCP(sessionBName, "127.0.0.1:5002", "token-b"))

	url, ok := readGeminiMCPServerURL(t, home, sessionAName)
	require.True(t, ok, "session B registration must not replace session A")
	require.Equal(t, "http://127.0.0.1:5001/mcp", url)
	require.Equal(t, "Bearer token-a", readMCPAuthorization(t, home, ".gemini", sessionAName))
	url, ok = readGeminiMCPServerURL(t, home, sessionBName)
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)
	require.Equal(t, "Bearer token-b", readMCPAuthorization(t, home, ".gemini", sessionBName))

	deregisterGeminiMCP(sessionAName, "127.0.0.1:5001")
	_, ok = readGeminiMCPServerURL(t, home, sessionAName)
	require.False(t, ok, "session A cleanup must remove only A")
	url, ok = readGeminiMCPServerURL(t, home, sessionBName)
	require.True(t, ok, "session A cleanup must preserve B")
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)
	deregisterGeminiMCP(sessionBName, "127.0.0.1:5002")
}

func TestDeregisterQwenMCP_DoesNotClobberNewerSession(t *testing.T) {
	home := isolateHomeDir(t)
	const serverName = "rush-test-shared"

	// Session A registers first.
	require.NoError(t, registerQwenMCP(serverName, "127.0.0.1:5001", "token-a"))

	// Session B (concurrent, same project+provider) overwrites with its
	// own URL — this is the existing, accepted "last write wins" behavior
	// for register; the bug this test targets is specifically about
	// deregister.
	require.NoError(t, registerQwenMCP(serverName, "127.0.0.1:5002", "token-b"))

	url, ok := readQwenMCPServerURL(t, home, serverName)
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:5002/mcp", url, "session B's registration must be the current one on disk")

	// Session A finishes and deregisters using ITS OWN (now-stale) addr —
	// must be a no-op, must NOT delete session B's still-active entry.
	deregisterQwenMCP(serverName, "127.0.0.1:5001")

	url, ok = readQwenMCPServerURL(t, home, serverName)
	require.True(t, ok, "session B's entry must still exist — session A's stale deregister must not have deleted it")
	require.Equal(t, "http://127.0.0.1:5002/mcp", url, "session B's entry must be untouched")

	// Session B finishes and deregisters using its OWN (current) addr —
	// this one must actually remove the entry.
	deregisterQwenMCP(serverName, "127.0.0.1:5002")

	_, ok = readQwenMCPServerURL(t, home, serverName)
	require.False(t, ok, "session B's own deregister, with the matching addr, must remove the entry")
}

func readGeminiMCPServerURL(t *testing.T, home, serverName string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json"))
	if os.IsNotExist(err) {
		return "", false
	}
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return "", false
	}
	entry, ok := mcpServers[serverName].(map[string]any)
	if !ok {
		return "", false
	}
	url, _ := entry["url"].(string)
	return url, true
}

func TestDeregisterGeminiMCP_DoesNotClobberNewerSession(t *testing.T) {
	home := isolateHomeDir(t)
	const serverName = "rush-test-shared"

	require.NoError(t, registerGeminiMCP(serverName, "127.0.0.1:5001", "token-a"))
	require.NoError(t, registerGeminiMCP(serverName, "127.0.0.1:5002", "token-b"))

	url, ok := readGeminiMCPServerURL(t, home, serverName)
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)

	// Session A's stale deregister must not clobber session B's entry.
	deregisterGeminiMCP(serverName, "127.0.0.1:5001")

	url, ok = readGeminiMCPServerURL(t, home, serverName)
	require.True(t, ok, "session B's entry must still exist")
	require.Equal(t, "http://127.0.0.1:5002/mcp", url)

	// Session B's own deregister, with the matching addr, must remove it.
	deregisterGeminiMCP(serverName, "127.0.0.1:5002")

	_, ok = readGeminiMCPServerURL(t, home, serverName)
	require.False(t, ok)
}
