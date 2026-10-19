package app

// R12-H2b chunk 4b, design §3 "Provider-backed crash procedure": the
// re-exec'd TEST BINARY child. The child opens a REAL App/store/host lock
// on the shared data dir, registers the witness-journal provider, submits
// one fs_write through PRODUCTION ingress (ExecuteRun with
// RunOverrides.DiskProvider + FolderScopes — never a direct batch_nodes
// insert) and either blocks INSIDE the provider forever (R12H2B_BLOCK=1)
// or completes the turn and exits 0 (R12H2B_BLOCK=0). The parent kills the
// blocked child mid-turn and recovers through production recovery.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
)

// providerCrashSignal is the JSON the blocked child writes to the signal
// file AFTER the witness journal entry is durable (flush ordering: the
// journal lands first, so a visible signal implies a visible record).
type providerCrashSignal struct {
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Session string `json:"session"`
	Target  string `json:"target"`
}

// providerCrashWitnessDisk wraps the package's reusable leakFakeDisk with
// the design's witness JOURNAL: every provider WriteFile first appends a
// unique record ("write <path> <content> #<n>") to R12H2B_WITNESS and
// Syncs it, and ONLY THEN (BLOCK=1) publishes the signal file and blocks
// forever inside the provider (select{} ignores ctx cancellation on
// purpose: the process must be killed mid-turn, never unwound cleanly).
type providerCrashWitnessDisk struct {
	*leakFakeDisk
	witnessPath string
	signalPath  string
	target      string
	hostID      string
	sessionID   string
	block       bool
	seq         atomic.Int64
}

func (d *providerCrashWitnessDisk) WriteFile(ctx context.Context, name string, b []byte, mode fs.FileMode) error {
	n := d.seq.Add(1)
	line := fmt.Sprintf("write %s %s #%d\n", filepath.ToSlash(name), string(b), n)
	f, err := os.OpenFile(d.witnessPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("witness journal open: %w", err)
	}
	if _, werr := f.WriteString(line); werr != nil {
		f.Close()
		return fmt.Errorf("witness journal write: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		return fmt.Errorf("witness journal sync: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("witness journal close: %w", cerr)
	}
	if d.block {
		// The signal is published ONLY after the journal record is synced.
		// Session rides along so the parent can scope the recovery sweep to
		// the child's owner session.
		sig, merr := json.Marshal(providerCrashSignal{
			PID: os.Getpid(), Host: d.hostID, Session: d.sessionID, Target: d.target,
		})
		if merr == nil {
			_ = os.WriteFile(d.signalPath, sig, 0o644)
		}
		select {} // blocked goroutine; the parent kills the whole process.
	}
	// BLOCK=0 variant: hold the leaf live for a moment so the parent can
	// observe the root row while it is still running, then let the turn
	// complete normally.
	time.Sleep(2 * time.Second)
	return d.leakFakeDisk.WriteFile(ctx, name, b, mode)
}

// providerCrashChildApp is the child's LOCAL builder (the shared
// newAdmissionRaceAppOnDir uses t.Setenv/t.Cleanup; the child needs a
// plain error-returning function over the env-provided dataDir so it can
// write a failure reason and os.Exit(2) without running test cleanup).
// The global-config isolation env (RUSH_GLOBAL_DATA/RUSH_GLOBAL_CONFIG/
// XDG_*) is provided by the PARENT through cmd.Env.
func providerCrashChildApp(dataDir string, handler http.HandlerFunc) (*App, string, error) {
	srv := httptest.NewServer(handler)

	store, err := config.Init(dataDir, dataDir, false)
	if err != nil {
		srv.Close()
		return nil, "", fmt.Errorf("config.Init: %w", err)
	}
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat", Model: "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat", Model: "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	if err != nil {
		srv.Close()
		return nil, "", fmt.Errorf("db.Connect: %w", err)
	}
	application, err := New(context.Background(), conn, store)
	if err != nil {
		srv.Close()
		return nil, "", fmt.Errorf("New: %w", err)
	}
	// A14 seam: match the shared fixture so no background title generation
	// fires and every provider request belongs to the main flow.
	agent.SetInlineWindowForTest(application.AgentCoordinator, false)

	sess, err := application.Sessions.Create(context.Background(), "provider-crash-child")
	if err != nil {
		return nil, "", fmt.Errorf("Sessions.Create: %w", err)
	}
	if _, err := application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	}); err != nil {
		return nil, "", fmt.Errorf("Messages.Create: %w", err)
	}
	return application, sess.ID, nil
}

// providerCrashChildHandler scripts the openaicompat SSE provider exactly
// like fsWriteScript: round 1 emits ONE fs_write tool call for the TARGET
// (absolute, native-spelled — the production fs_* family hands the
// provider the slashed form); the follow-up round (recognized by the
// tool-call id) returns text+stop. With R12H2B_BLOCK=1 the provider never
// returns, so the follow-up round is never requested.
func providerCrashChildHandler(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"call_w"`) {
			admissionWriteSSE(w, []string{
				admissionSSEText("c2", "CRASH_HELPER_DONE"),
				admissionSSEStop("c2", "stop"),
			})
			return
		}
		input, err := json.Marshal(map[string]any{
			"items": []map[string]any{
				{"path": filepath.ToSlash(target), "content": "child-value"},
			},
		})
		if err != nil {
			return
		}
		admissionWriteSSE(w, []string{
			admissionSSEToolCall("c1", "call_w", "fs_write", string(input)),
			admissionSSEStop("c1", "tool_calls"),
		})
	}
}

// runProviderCrashHelper is the child body. It MUST os.Exit(0) explicitly
// when ExecuteRun ever returns (the BLOCK=0 completion variant); on any
// setup failure it writes the reason to R12H2B_REASON and exits 2 so the
// parent can surface it.
func runProviderCrashHelper(t *testing.T) {
	t.Helper()
	dataDir := os.Getenv("R12H2B_DATA_DIR")
	witness := os.Getenv("R12H2B_WITNESS")
	signal := os.Getenv("R12H2B_SIGNAL")
	target := os.Getenv("R12H2B_TARGET")
	block := os.Getenv("R12H2B_BLOCK") == "1"
	done := signal + ".done"
	fail := func(msg string) {
		_ = db.Release(dataDir) // no-op (returns nil) if dataDir was never Connect'ed
		_ = os.WriteFile(os.Getenv("R12H2B_REASON"), []byte(msg), 0o644)
		os.Exit(2)
	}
	if dataDir == "" || witness == "" || signal == "" || target == "" {
		fail("missing crash-helper env")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fail("scope dir: " + err.Error())
	}

	application, sessID, err := providerCrashChildApp(dataDir, providerCrashChildHandler(target))
	if err != nil {
		fail("child app: " + err.Error())
	}
	hostID := ""
	if application.batchHostStore() != nil {
		hostID = application.batchHostStore().HostID()
	}
	if hostID == "" {
		fail("the flagged install registered no host identity")
	}
	// Identity info file: published BEFORE the run starts so the parent can
	// poll the child's root row and scope its recovery sweep while the
	// child is still alive.
	if werr := os.WriteFile(signal+".info", []byte(sessID+" "+hostID), 0o644); werr != nil {
		fail("info signal: " + werr.Error())
	}

	scopeDir := filepath.Dir(target)
	fake := &providerCrashWitnessDisk{
		leakFakeDisk: newLeakFakeDisk(),
		witnessPath:  witness,
		signalPath:   signal,
		target:       target,
		hostID:       hostID,
		sessionID:    sessID,
		block:        block,
	}
	// Seed the scoped ancestor under BOTH spellings exactly like the leak
	// and shutdown fixtures (the fs_* ancestor walk may Stat either form).
	fake.seedDir(scopeDir)
	fake.seedDir(filepath.ToSlash(scopeDir))

	_, err = application.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "provider crash probe",
		Mode:              RunModeJSON,
		ContinueSessionID: sessID,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		Overrides: RunOverrides{
			FolderScopes: []permission.FolderScopeEntry{{
				Dir: scopeDir,
				Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite, permission.FileOpRead},
			}},
			DiskProvider: fake,
		},
	})
	if block {
		// The provider never returns mid-crash; if ExecuteRun somehow did
		// return, still never let the child exit cleanly.
		select {}
	}
	if err != nil {
		fail("ExecuteRun: " + err.Error())
	}
	// Child hygiene: a COMPLETED run exits 0 explicitly, after publishing
	// the success signal (session + host) the parent waits for.
	if werr := os.WriteFile(done, []byte(sessID+" "+hostID), 0o644); werr != nil {
		fail("done signal: " + werr.Error())
	}
	// The child exits via os.Exit (defers never run): release the pool refCount explicitly.
	_ = db.Release(dataDir)
	os.Exit(0)
}
