package cliprovider

// Registration of the rush MCP server in external CLI settings files
// (~/.qwen/settings.json and ~/.gemini/settings.json): stable per-project
// server IDs, register/deregister pairs, and the flock helpers that
// serialise settings read-modify-write cycles across parallel rush runs.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/google/uuid"
)

// mcpConfigLockTimeout caps how long an MCP id/settings flock will
// wait before failing. Fork patch (concurrency): chosen so a wedged
// sibling rush process (debugger, suspended shell, frozen NFS mount)
// cannot freeze the entire parallel-run fleet on a shared id/settings
// file — see CHANGELOG.fork.md (Section 4.I).
const mcpConfigLockTimeout = 30 * time.Second

// acquireMCPConfigLock is a thin wrapper around
// session.AcquireFileLockContext that enforces mcpConfigLockTimeout.
// All MCP id/settings critical sections in this file use it.
func acquireMCPConfigLock(lockPath string) (*session.FileLock, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpConfigLockTimeout)
	defer cancel()
	return session.AcquireFileLockContext(ctx, lockPath)
}

// ── qwen MCP registration ─────────────────────────────────────────────────────

// qwenMCPID returns a stable MCP server name for the given workingDir.
// If <workingDir>/.rush/ already exists, the ID is stored there in qwen-mcp-id.
// Otherwise a temp file keyed by workingDir is used so we never create .rush/
// in directories that don't already have a rush project.
//
// Fork patch (concurrency): wrap the read-then-write of the id file with
// a flock (session.AcquireFileLock) so two parallel `rush run` processes
// in the same workingDir cannot both miss the file, both generate a UUID,
// and end up with a split-brain MCP server name. See CHANGELOG.fork.md.
func qwenMCPID(workingDir string) (string, error) {
	var idFile string
	crushDir := filepath.Join(workingDir, ".rush")
	if info, err := os.Stat(crushDir); err == nil && info.IsDir() {
		// .rush/ exists — this is a rush project directory, store ID there.
		idFile = filepath.Join(crushDir, "qwen-mcp-id")
	} else {
		// No .rush/ here — use a temp file keyed by a hash of the path so
		// the ID remains stable across rush restarts without polluting the dir.
		h := fmt.Sprintf("%x", []byte(workingDir))
		if len(h) > 16 {
			h = h[:16]
		}
		idFile = filepath.Join(os.TempDir(), "rush-qwen-mcp-"+h)
	}
	// Fork patch: serialise the read-modify-write below across processes.
	lock, err := acquireMCPConfigLock(idFile + ".lock")
	if err != nil {
		return "", fmt.Errorf("cliprovider: lock qwen-mcp-id: %w", err)
	}
	defer lock.Release()
	if data, err := os.ReadFile(idFile); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}
	// Generate a short stable ID for this project.
	id := "rush-" + uuid.New().String()[:8]
	if err := fsext.AtomicWriteFile(idFile, []byte(id), 0o644); err != nil {
		return "", fmt.Errorf("cliprovider: write qwen-mcp-id: %w", err)
	}
	slog.Info("cliprovider: created qwen MCP ID", "id", id, "file", idFile)
	return id, nil
}

// qwenSettingsPath returns the path to ~/.qwen/settings.json.
func qwenSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".qwen", "settings.json"), nil
}

// registerQwenMCP adds the rush MCP server to ~/.qwen/settings.json.
// It removes any stale entry with the same name first, then writes the new URL.
// The Authorization: Bearer header is stored in the settings so Qwen sends it
// with each MCP request.
//
// Fork patch (concurrency): the read-modify-write of settings.json is
// guarded by a sibling .lock file so parallel `rush run` processes (or
// concurrent rush + qwen invocations) cannot stomp each other's
// entries, and the write itself is atomic so a kill mid-write cannot
// leave a half-truncated settings.json. See CHANGELOG.fork.md.
func registerQwenMCP(serverName, addr, token string) error {
	path, err := qwenSettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("cliprovider: mkdir qwen settings dir: %w", err)
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return fmt.Errorf("cliprovider: lock qwen settings: %w", err)
	}
	defer lock.Release()
	var settings map[string]any
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpServers[serverName] = map[string]any{
		"httpUrl": "http://" + addr + "/mcp",
		"headers": map[string]string{
			"Authorization": "Bearer " + token,
		},
	}
	settings["mcpServers"] = mcpServers
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	slog.Info("cliprovider: registered qwen MCP server", "name", serverName, "addr", addr)
	return fsext.AtomicWriteFile(path, data, 0o644)
}

// deregisterQwenMCP removes the rush MCP entry from ~/.qwen/settings.json,
// but ONLY if it still points at expectedAddr — the exact addr this specific
// call's own registerQwenMCP wrote.
//
// serverName is a STABLE per-workingDir ID (see qwenMCPID), so two
// concurrent rush sessions in the same project both integrating with Qwen
// share one mcpServers[serverName] entry: registerQwenMCP unconditionally
// overwrites it with whichever session called last, and an unconditional
// delete here used to remove the entry regardless of which session
// currently owned it — found by a full-project @crush --role reviewer
// audit. Session A finishing first would delete session B's still-active
// registration (B's own overwrite already replaced A's url with B's own),
// breaking qwen's ability to reconnect to B's MCP server for the rest of
// B's session. Comparing the stored url first makes this call a safe no-op
// once a later session has taken over the shared name, instead of
// clobbering that later session's entry.
//
// Fork patch (concurrency): same flock + atomic-write as registerQwenMCP.
func deregisterQwenMCP(serverName, expectedAddr string) {
	path, err := qwenSettingsPath()
	if err != nil {
		return
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var settings map[string]any
	if json.Unmarshal(data, &settings) != nil {
		return
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return
	}
	entry, ok := mcpServers[serverName].(map[string]any)
	if !ok {
		return
	}
	if storedURL, _ := entry["httpUrl"].(string); storedURL != "http://"+expectedAddr+"/mcp" {
		slog.Debug("cliprovider: qwen MCP entry no longer ours, leaving it for its current owner", "name", serverName)
		return
	}
	delete(mcpServers, serverName)
	if len(mcpServers) == 0 {
		delete(settings, "mcpServers")
	} else {
		settings["mcpServers"] = mcpServers
	}
	if data, err = json.MarshalIndent(settings, "", "  "); err != nil {
		return
	}
	_ = fsext.AtomicWriteFile(path, data, 0o644)
	slog.Info("cliprovider: deregistered qwen MCP server", "name", serverName)
}

// ── gemini MCP registration ───────────────────────────────────────────────────

// geminiMCPID returns a stable MCP server name for the given workingDir.
// Mirrors the logic of qwenMCPID but uses a separate ID file (gemini-mcp-id).
//
// Fork patch (concurrency): same flock + atomic-write treatment as
// qwenMCPID — see that function's note.
func geminiMCPID(workingDir string) (string, error) {
	var idFile string
	crushDir := filepath.Join(workingDir, ".rush")
	if info, err := os.Stat(crushDir); err == nil && info.IsDir() {
		idFile = filepath.Join(crushDir, "gemini-mcp-id")
	} else {
		h := fmt.Sprintf("%x", []byte(workingDir))
		if len(h) > 16 {
			h = h[:16]
		}
		idFile = filepath.Join(os.TempDir(), "rush-gemini-mcp-"+h)
	}
	lock, err := acquireMCPConfigLock(idFile + ".lock")
	if err != nil {
		return "", fmt.Errorf("cliprovider: lock gemini-mcp-id: %w", err)
	}
	defer lock.Release()
	if data, err := os.ReadFile(idFile); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}
	id := "rush-" + uuid.New().String()[:8]
	if err := fsext.AtomicWriteFile(idFile, []byte(id), 0o644); err != nil {
		return "", fmt.Errorf("cliprovider: write gemini-mcp-id: %w", err)
	}
	slog.Info("cliprovider: created gemini MCP ID", "id", id, "file", idFile)
	return id, nil
}

// geminiSettingsPath returns the path to ~/.gemini/settings.json.
func geminiSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "settings.json"), nil
}

// registerGeminiMCP adds the rush MCP server to ~/.gemini/settings.json.
// The Authorization: Bearer header is stored in the settings so Gemini sends
// it with each MCP request. trust:true bypasses Gemini's own confirmation
// prompts so tool calls flow directly to rush's permission dialog.
// Fork patch (concurrency): flock + atomic-write — see registerQwenMCP.
func registerGeminiMCP(serverName, addr, token string) error {
	path, err := geminiSettingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return fmt.Errorf("cliprovider: lock gemini settings: %w", err)
	}
	defer lock.Release()
	var settings map[string]any
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &settings)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpServers[serverName] = map[string]any{
		"url":  "http://" + addr + "/mcp",
		"type": "http",
		"headers": map[string]string{
			"Authorization": "Bearer " + token,
		},
		"trust": true,
	}
	settings["mcpServers"] = mcpServers
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	slog.Info("cliprovider: registered gemini MCP server", "name", serverName, "addr", addr)
	return fsext.AtomicWriteFile(path, data, 0o644)
}

// deregisterGeminiMCP removes the rush MCP entry from
// ~/.gemini/settings.json, but ONLY if it still points at expectedAddr —
// the exact addr this specific call's own registerGeminiMCP wrote. See
// deregisterQwenMCP's doc for why an unconditional delete is unsafe when
// two concurrent sessions in the same project share one stable server name
// (serverName, from geminiMCPID).
//
// Fork patch (concurrency): flock + atomic-write — see registerQwenMCP.
func deregisterGeminiMCP(serverName, expectedAddr string) {
	path, err := geminiSettingsPath()
	if err != nil {
		return
	}
	lock, err := acquireMCPConfigLock(path + ".lock")
	if err != nil {
		return
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var settings map[string]any
	if json.Unmarshal(data, &settings) != nil {
		return
	}
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return
	}
	entry, ok := mcpServers[serverName].(map[string]any)
	if !ok {
		return
	}
	if storedURL, _ := entry["url"].(string); storedURL != "http://"+expectedAddr+"/mcp" {
		slog.Debug("cliprovider: gemini MCP entry no longer ours, leaving it for its current owner", "name", serverName)
		return
	}
	delete(mcpServers, serverName)
	if len(mcpServers) == 0 {
		delete(settings, "mcpServers")
	} else {
		settings["mcpServers"] = mcpServers
	}
	if data, err = json.MarshalIndent(settings, "", "  "); err != nil {
		return
	}
	_ = fsext.AtomicWriteFile(path, data, 0o644)
	slog.Info("cliprovider: deregistered gemini MCP server", "name", serverName)
}
