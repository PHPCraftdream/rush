package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runRunCommandTool marshals params and runs the tool. Requiring a nil Go
// error here IS the error-contract test for this tool: every
// model-correctable failure must come back as a response instead.
func runRunCommandTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params RunCommandParams) fantasy.ToolResponse {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "c1",
		Name:  RunCommandToolName,
		Input: string(raw),
	})
	require.NoError(t, err)
	return resp
}

func newRunCommandToolForTest(t *testing.T, workingDir string) fantasy.AgentTool {
	t.Helper()
	perms := &recordingPermissionService{
		Broker: pubsub.NewBroker[permission.PermissionRequest](),
		allow:  true,
	}
	return NewRunCommandTool(perms, workingDir)
}

func TestRunCommand_HappyPath(t *testing.T) {
	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "go",
		Args:    []string{"version"},
	})
	require.False(t, resp.IsError, resp.Content)
	assert.Contains(t, resp.Content, "go version")
}

// TestRunCommand_NoShellInterpretation is THE decisive security test: it
// proves the argv reaches the OS process-creation call directly, with no
// shell parsing in between. Each injected element contains a shell
// metacharacter that, under a shell, would execute a second command and
// delete the victim file. The file must survive in every case; the target
// program rejecting the bogus literal operand is the documented acceptable
// outcome.
func TestRunCommand_NoShellInterpretation(t *testing.T) {
	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	t.Run("semicolon", func(t *testing.T) {
		tmpFile := filepath.Join(t.TempDir(), "victim.txt")
		require.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0o644))
		resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
			Program: "go",
			Args:    []string{"version", "; rm -f " + tmpFile},
		})
		_, statErr := os.Stat(tmpFile)
		require.NoError(t, statErr, "victim file must survive: %s", resp.Content)
		// Note: go rejects the bogus literal operand and its error text
		// echoes it — that echo is not evidence of a second command; the
		// surviving file above is the proof.
	})

	t.Run("command-substitution", func(t *testing.T) {
		tmpFile := filepath.Join(t.TempDir(), "victim.txt")
		require.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0o644))
		resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
			Program: "go",
			Args:    []string{"version", "$(" + tmpFile + ")"},
		})
		_, statErr := os.Stat(tmpFile)
		require.NoError(t, statErr, "victim file must survive: %s", resp.Content)
	})

	t.Run("pipe", func(t *testing.T) {
		tmpFile := filepath.Join(t.TempDir(), "victim.txt")
		require.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0o644))
		resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
			Program: "git",
			Args:    []string{"log", "--oneline", "| del " + tmpFile},
		})
		_, statErr := os.Stat(tmpFile)
		require.NoError(t, statErr, "victim file must survive: %s", resp.Content)
		assert.NotContains(t, resp.Content, "del")
	})
}

func TestRunCommand_BannedProgram(t *testing.T) {
	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "curl",
		Args:    []string{"https://invalid"},
	})
	require.True(t, resp.IsError, "curl must be rejected")
	assert.Contains(t, resp.Content, "not allowed")

	resp = runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "sudo",
		Args:    []string{"ls"},
	})
	require.True(t, resp.IsError, "sudo must be rejected")
	assert.Contains(t, resp.Content, "not allowed")

	// Argument-blocker parity with the bash tool: `go install` is blocked
	// even though the program itself is fine.
	resp = runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "go",
		Args:    []string{"install", "example.com/x"},
	})
	require.True(t, resp.IsError, "go install must be rejected")
	assert.Contains(t, resp.Content, "not allowed")
}

func TestRunCommand_WorkingDirContainment(t *testing.T) {
	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program:    "go",
		Args:       []string{"version"},
		WorkingDir: "../outside",
	})
	require.True(t, resp.IsError, "relative escape must be rejected")
	assert.Contains(t, resp.Content, "inside the working directory")

	resp = runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program:    "go",
		Args:       []string{"version"},
		WorkingDir: t.TempDir(),
	})
	require.True(t, resp.IsError, "absolute outside path must be rejected")
	assert.Contains(t, resp.Content, "inside the working directory")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	resp = runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program:    "go",
		Args:       []string{"version"},
		WorkingDir: "sub",
	})
	require.False(t, resp.IsError, resp.Content)
	var meta RunCommandResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	assert.True(t, strings.HasSuffix(filepath.ToSlash(meta.WorkingDirectory), "/sub"),
		"metadata working directory should end with sub, got %q", meta.WorkingDirectory)
}

func TestRunCommand_TimeoutKills(t *testing.T) {
	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	params := RunCommandParams{TimeoutSeconds: 1}
	if runtime.GOOS == "windows" {
		params.Program = "ping"
		params.Args = []string{"-n", "30", "127.0.0.1"}
	} else {
		params.Program = "sleep"
		params.Args = []string{"30"}
	}

	start := time.Now()
	resp := runRunCommandTool(t, tool, ctx, params)
	elapsed := time.Since(start)

	require.True(t, resp.IsError, "expected timeout error response")
	assert.Contains(t, resp.Content, "timed out")
	assert.Less(t, elapsed, 15*time.Second, "timeout must actually kill the program")
}

func TestRunCommand_OutputTruncated(t *testing.T) {
	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	mainGo := filepath.Join(dir, "main.go")
	src := "package main\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n" +
		"func main() {\n\tfor i := 0; i < 4000; i++ {\n\t\tfmt.Println(strings.Repeat(\"x\", 80))\n\t}\n}\n"
	require.NoError(t, os.WriteFile(mainGo, []byte(src), 0o644))

	resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "go",
		Args:    []string{"run", mainGo},
	})
	assert.Contains(t, resp.Content, "lines truncated")
	var meta RunCommandResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	assert.LessOrEqual(t, len(meta.Output), MaxOutputLength+200)
}

func TestRunCommand_ConcurrentDifferentWorkingDirs(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(rootA, "a"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(rootB, "b"), 0o755))

	tool1 := newRunCommandToolForTest(t, rootA)
	tool2 := newRunCommandToolForTest(t, rootB)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	type result struct {
		meta RunCommandResponseMetadata
		resp fantasy.ToolResponse
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		resp := runRunCommandTool(t, tool1, ctx, RunCommandParams{
			Program: "go", Args: []string{"version"}, WorkingDir: "a",
		})
		var meta RunCommandResponseMetadata
		_ = json.Unmarshal([]byte(resp.Metadata), &meta)
		results[0] = result{meta, resp}
	}()
	go func() {
		defer wg.Done()
		resp := runRunCommandTool(t, tool2, ctx, RunCommandParams{
			Program: "go", Args: []string{"version"}, WorkingDir: "b",
		})
		var meta RunCommandResponseMetadata
		_ = json.Unmarshal([]byte(resp.Metadata), &meta)
		results[1] = result{meta, resp}
	}()
	wg.Wait()

	for i, root := range []string{rootA, rootB} {
		require.False(t, results[i].resp.IsError, results[i].resp.Content)
		require.Contains(t, filepath.ToSlash(results[i].meta.WorkingDirectory), filepath.ToSlash(root),
			"run %d must have executed inside its own root", i)
	}
}

func TestRunCommand_ProgramNotFound(t *testing.T) {
	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "definitely-not-a-real-binary-xyz",
	})
	require.True(t, resp.IsError)
	assert.Contains(t, resp.Content, "not found")
}

// recordingRunCommandPermissionService wraps the shared
// recordingPermissionService and keeps the last request for inspection.
type recordingRunCommandPermissionService struct {
	*recordingPermissionService

	mu         sync.Mutex
	lastParams permission.CreatePermissionRequest
}

func (m *recordingRunCommandPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.mu.Lock()
	m.lastParams = req
	m.mu.Unlock()
	return m.recordingPermissionService.Request(ctx, req)
}

func TestRunCommand_PermissionRequested(t *testing.T) {
	dir := t.TempDir()
	perms := &recordingRunCommandPermissionService{
		recordingPermissionService: &recordingPermissionService{
			Broker: pubsub.NewBroker[permission.PermissionRequest](),
			allow:  true,
		},
	}
	tool := NewRunCommandTool(perms, dir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	resp := runRunCommandTool(t, tool, ctx, RunCommandParams{
		Program: "go",
		Args:    []string{"version"},
	})
	require.False(t, resp.IsError, resp.Content)
	require.Equal(t, 1, perms.requestCount)

	require.Equal(t, RunCommandToolName, perms.lastParams.ToolName)
	rp, ok := perms.lastParams.Params.(RunCommandPermissionsParams)
	require.True(t, ok, "Params must be RunCommandPermissionsParams, got %T", perms.lastParams.Params)
	require.Equal(t, "go version", rp.RunAllowlistCommand())
}

// TestRunCommandPermissionsParams_RunAllowlistCommandContract pins how
// RunCommandPermissionsParams integrates with the real permission service.
// The generalized gate routes ANY tool whose params implement
// RunAllowlistCommand through allow_bash-pattern scrutiny (see
// internal/permission/runallowlist.go), so requests under
// RunCommandToolName get command-level scrutiny — an AllowTools entry for
// "run_command" cannot authorize commands on its own.
func TestRunCommandPermissionsParams_RunAllowlistCommandContract(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	svc := permission.NewPermissionService(t.Context(), "/tmp", false, nil, db.New(conn))
	svc.AutoApproveSession("run-session")
	allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
		Restrict:   true,
		AllowBash:  []string{"go test"},
		AllowTools: []string{"run_command"},
	})
	require.NoError(t, err)
	svc.SetRunAllowlist(allowlist)

	// Command-pattern scrutiny via RunAllowlistCommand: "go test ./..."
	// matches the "go test" prefix pattern.
	allowed, err := svc.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID: "run-session",
		ToolName:  RunCommandToolName,
		Action:    "execute",
		Path:      "/tmp",
		Params:    RunCommandPermissionsParams{Program: "go", Args: []string{"test", "./..."}},
	})
	require.NoError(t, err)
	require.True(t, allowed)

	denied, err := svc.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID: "run-session",
		ToolName:  RunCommandToolName,
		Action:    "execute",
		Path:      "/tmp",
		Params:    RunCommandPermissionsParams{Program: "rm", Args: []string{"-rf", "x"}},
	})
	require.NoError(t, err)
	require.False(t, denied)

	// A run_command AllowTools entry with NO AllowBash patterns must NOT
	// authorize commands — command scrutiny only.
	allowlistToolsOnly, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
		Restrict:   true,
		AllowTools: []string{"run_command"},
	})
	require.NoError(t, err)
	svc.SetRunAllowlist(allowlistToolsOnly)
	deniedToolsOnly, err := svc.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID: "run-session",
		ToolName:  RunCommandToolName,
		Action:    "execute",
		Path:      "/tmp",
		Params:    RunCommandPermissionsParams{Program: "go", Args: []string{"test", "./..."}},
	})
	require.NoError(t, err)
	require.False(t, deniedToolsOnly)

	// AllowBash alone, with NO AllowTools entry for "run_command" at all,
	// must still authorize a matching command — the whole point of the
	// generalization is that command-pattern scrutiny does not depend on
	// a tool-name allowlist entry.
	allowlistNoTools, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
		Restrict:  true,
		AllowBash: []string{"go test"},
	})
	require.NoError(t, err)
	svc.SetRunAllowlist(allowlistNoTools)
	allowedByBashOnly, err := svc.Request(t.Context(), permission.CreatePermissionRequest{
		SessionID: "run-session",
		ToolName:  RunCommandToolName,
		Action:    "execute",
		Path:      "/tmp",
		Params:    RunCommandPermissionsParams{Program: "go", Args: []string{"test", "./..."}},
	})
	require.NoError(t, err)
	require.True(t, allowedByBashOnly)
}

func TestRunCommandPermissionsParams_Quoting(t *testing.T) {
	assert.Equal(t, "go test ./...",
		RunCommandPermissionsParams{Program: "go", Args: []string{"test", "./..."}}.RunAllowlistCommand())
	assert.Equal(t, "echo 'a b'",
		RunCommandPermissionsParams{Program: "echo", Args: []string{"a b"}}.RunAllowlistCommand())
	assert.Equal(t, "go",
		RunCommandPermissionsParams{Program: "go"}.RunAllowlistCommand())
	assert.Equal(t, "",
		RunCommandPermissionsParams{}.RunAllowlistCommand())
}
