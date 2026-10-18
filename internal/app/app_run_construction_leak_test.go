package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type leakFakeDisk struct {
	mu     sync.Mutex
	files  map[string]string
	dirs   map[string]struct{}
	closed int
}

func newLeakFakeDisk() *leakFakeDisk {
	return &leakFakeDisk{files: map[string]string{}, dirs: map[string]struct{}{}}
}

func (d *leakFakeDisk) seedDir(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		d.dirs[dir] = struct{}{}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}
func (d *leakFakeDisk) Close() error { d.mu.Lock(); defer d.mu.Unlock(); d.closed++; return nil }
func (d *leakFakeDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.dirs[name]; ok {
		return leakDirInfo{}, nil
	}
	if _, ok := d.files[name]; ok {
		return leakFileInfo{}, nil
	}
	return nil, fs.ErrNotExist
}

type leakDirInfo struct{}

func (leakDirInfo) Name() string       { return "dir" }
func (leakDirInfo) Size() int64        { return 0 }
func (leakDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (leakDirInfo) ModTime() time.Time { return time.Time{} }
func (leakDirInfo) IsDir() bool        { return true }
func (leakDirInfo) Sys() any           { return nil }

type leakFileInfo struct{}

func (leakFileInfo) Name() string       { return "file" }
func (leakFileInfo) Size() int64        { return 0 }
func (leakFileInfo) Mode() fs.FileMode  { return 0o644 }
func (leakFileInfo) ModTime() time.Time { return time.Time{} }
func (leakFileInfo) IsDir() bool        { return false }
func (leakFileInfo) Sys() any           { return nil }

func (d *leakFakeDisk) EvalSymlinks(_ context.Context, name string) (string, error) { return name, nil }

func (d *leakFakeDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return io.NopCloser(strings.NewReader(d.files[name])), nil
}

func (d *leakFakeDisk) ReadFile(ctx context.Context, name string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return []byte(d.files[name]), nil
}
func (d *leakFakeDisk) MkdirAll(_ context.Context, name string, _ fs.FileMode) error { return nil }
func (d *leakFakeDisk) WriteFile(_ context.Context, name string, b []byte, _ fs.FileMode) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[name] = string(b)
	return nil
}

func (d *leakFakeDisk) Remove(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.files, name)
	return nil
}

func (d *leakFakeDisk) List(context.Context, tools.ListRequest) (tools.ListResult, error) {
	return tools.ListResult{}, nil
}

func (d *leakFakeDisk) Find(context.Context, tools.FindRequest) (tools.FindResult, error) {
	return tools.FindResult{}, nil
}

func (d *leakFakeDisk) Search(context.Context, tools.SearchRequest) (tools.DiskSearchResult, error) {
	return tools.DiskSearchResult{}, nil
}

func TestSDKRunConstructionOwnersReturnToEmpty(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	var target string
	application, _ := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"call_read"`):
			admissionWriteSSE(w, []string{admissionSSEText("c3", "readback: durable-value"), admissionSSEStop("c3", "stop")})
		case strings.Contains(string(body), `"call_write"`):
			input, err := json.Marshal(map[string]any{"items": []map[string]any{{"path": target}}})
			require.NoError(t, err)
			admissionWriteSSE(w, []string{admissionSSEToolCall("c2", "call_read", "fs_read", string(input)), admissionSSEStop("c2", "tool_calls")})
		default:
			input, err := json.Marshal(map[string]any{"items": []map[string]any{{"path": target, "content": "durable-value"}}})
			require.NoError(t, err)
			admissionWriteSSE(w, []string{admissionSSEToolCall("c1", "call_write", "fs_write", string(input)), admissionSSEStop("c1", "tool_calls")})
		}
	})
	require.NotNil(t, application.batchInstall)
	cutoverWriter := application.batchCutoverWriter()
	agent.BindCoordinatorBatchCutoverFacts(application.AgentCoordinator,
		agent.NewBatchCutoverNodeFacts(session.NewBatchStore(cutoverWriter), application.batchLiveReader, cutoverWriter))
	workDir := application.config.WorkingDir()
	conn := application.DB()
	dir := filepath.Join(workDir, "scope")
	fake := newLeakFakeDisk()
	fake.seedDir(dir)
	fake.seedDir(filepath.ToSlash(dir))
	for run := 1; run <= 3; run++ {
		sess, err := application.Sessions.Create(context.Background(), fmt.Sprintf("construction-leak-%d", run))
		require.NoError(t, err)
		sessionID := sess.ID
		target = filepath.ToSlash(filepath.Join(dir, fmt.Sprintf("probe-%d.txt", run)))
		res, err := application.ExecuteRun(context.Background(), RunRequest{
			Prompt: "write and read probe", Mode: RunModeJSON, ContinueSessionID: sessionID,
			Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true,
			Overrides: RunOverrides{
				FolderScopes: []permission.FolderScopeEntry{{Dir: dir, Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead}}},
				DiskProvider: fake,
			},
		})
		require.NoError(t, err, "run %d", run)
		require.NotNil(t, res)
		require.Equal(t, "end_turn", res.ExitReason)
		require.Contains(t, res.FinalText, "readback: durable-value")
		fake.mu.Lock()
		// The production fs tool layer canonicalizes paths to the NATIVE
		// OS form before the provider WriteFile call, so the provider's
		// key is the native form of the slashed target on Windows.
		contents, ok := fake.files[target]
		if !ok {
			contents = fake.files[filepath.FromSlash(target)]
		}
		fake.mu.Unlock()
		_ = ok
		require.Equal(t, "durable-value", contents, "run %d must persist provider write", run)
		messages, err := application.Messages.List(context.Background(), sessionID)
		require.NoError(t, err)
		readResult := false
		for _, msg := range messages {
			for _, part := range msg.Parts {
				if result, ok := part.(message.ToolResult); ok && result.Name == "fs_read" {
					require.False(t, result.IsError, "run %d fs_read failed: %s", run, result.Content)
					require.Contains(t, result.Content, "durable-value")
					readResult = true
				}
			}
		}
		require.True(t, readResult, "run %d must persist a successful fs_read result", run)
		var roots int
		require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM batch_nodes WHERE owner_session_id = ?1 AND kind = 'task'`, sessionID).Scan(&roots))
		require.GreaterOrEqual(t, roots, 2, "run %d must create write and read batch roots", run)
		require.Eventually(t, func() bool {
			owners, constructs, registries := application.batchInstall.DiskBindings.OwnerDiagnostics()
			return owners == 0 && constructs == 0 && registries == 0 && application.batchInstall.Runtime.RegistryCount() == 0
		}, 5*time.Second, 10*time.Millisecond, "run %d capability owners and registry entries must return exactly to empty", run)
	}
}

var _ tools.DiskProvider = (*leakFakeDisk)(nil)
