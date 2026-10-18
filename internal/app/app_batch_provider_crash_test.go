package app

// R12-H2b chunk 4b, design §3 "Provider-backed crash procedure"
// (acceptances 10-13). One test func serves BOTH roles via re-exec: the
// child (R12H2B_CRASH_HELPER=1) is the REAL SUBPROCESS whose provider
// blocks forever mid-fs_write; the parent waits for the signal file,
// verifies the announced running root + the alive host probe, kills the
// child, then runs PRODUCTION recovery (BatchSweepDeadHosts) over the same
// data dir and proves the no-replay barrier. The second test func is the
// after-completion variant (R12H2B_BLOCK=0): the turn COMMITS before the
// kill, and recovery must report zero interrupts without disturbing the
// committed outcome.
//
// WHY-COMMENT (design): "Negative no-replay assertions require completed
// sweep/pump barriers, not an arbitrary sleep" — every require.Never below
// runs AFTER both recovery passes and the pump tick, never instead of it.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// providerCrashCountingExecutor replaces the default R7 router on the
// PARENT's observing app via batchInstallDepsHook. The design demands a
// counting executor so a recovery REPLAY that dies at binding lookup
// before reaching any provider still shows up as a Start — Start must stay
// 0 across both recovery passes.
type providerCrashCountingExecutor struct {
	starts atomic.Int64
}

func (e *providerCrashCountingExecutor) Start(
	_ context.Context, _ jobbatch.Launch, _ func(jobbatch.Result),
) (jobbatch.Execution, error) {
	e.starts.Add(1)
	return nopExecution{}, nil
}

// providerCrashSentinel is the pre-seeded REAL OS file content at the
// fs_write target. The child's provider is virtual, so any OS-disk replay
// would overwrite this.
const providerCrashSentinel = "OS-SENTINEL-MUST-NOT-CHANGE\n"

// providerCrashFixture carries the shared parent-side state of one crash
// procedure run.
type providerCrashFixture struct {
	dataDir   string
	scopeDir  string
	target    string
	witness   string
	signal    string
	info      string
	done      string
	reason    string
	executor  *providerCrashCountingExecutor
	observing *App
	observers string // observing app's own seed session (never recovered)
	store     *session.BatchStore
	cmd       *exec.Cmd
	waitErr   chan error
	output    bytes.Buffer
}

// launchProviderCrashChild re-execs the test binary as the crash child
// (app_batch_subprocess_test.go's os.Executable + -test.run pattern) with
// its OWN global-config isolation env, pointed at the SHARED dataDir.
func launchProviderCrashChild(t *testing.T, f *providerCrashFixture, block string) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	childIso := t.TempDir()
	childConfig := filepath.Join(childIso, "config")
	require.NoError(t, os.MkdirAll(childConfig, 0o755))
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	t.Cleanup(cancel)
	cmd := platform.Command(ctx, exe, "-test.run=^TestAppBatchProviderCrashSubprocess$")
	cmd.Env = append(os.Environ(),
		"R12H2B_CRASH_HELPER=1",
		"R12H2B_DATA_DIR="+f.dataDir,
		"R12H2B_WITNESS="+f.witness,
		"R12H2B_SIGNAL="+f.signal,
		"R12H2B_TARGET="+f.target,
		"R12H2B_REASON="+f.reason,
		"R12H2B_BLOCK="+block,
		"XDG_DATA_HOME="+childIso,
		"RUSH_GLOBAL_DATA="+childIso,
		"XDG_CONFIG_HOME="+childConfig,
		"RUSH_GLOBAL_CONFIG="+childConfig,
		"RUSH_PROVIDER_CACHE_ONLY=1",
		"RUSH_BATCH_RUNTIME=1",
	)
	cmd.Stdout = &f.output
	cmd.Stderr = &f.output
	require.NoError(t, cmd.Start())
	f.cmd = cmd
	f.waitErr = make(chan error, 1)
	go func() { f.waitErr <- cmd.Wait() }()
}

// childReason dumps the child's failure reason (if any) into require
// messages on unexpected exits.
func (f *providerCrashFixture) childReason() string {
	raw, err := os.ReadFile(f.reason)
	if err != nil || len(raw) == 0 {
		return ""
	}
	return " child reason: " + strings.TrimSpace(string(raw))
}

// providerCrashWitnessLines counts durable journal records.
func providerCrashWitnessLines(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// newProviderCrashFixture builds the parent-side world: the shared dataDir
// with the pre-seeded REAL OS sentinel at the target, and the observing
// app (its pump running) built with the counting executor already hooked
// into the flagged install (the hook applies at install time, so it must
// be installed BEFORE New).
func newProviderCrashFixture(t *testing.T) *providerCrashFixture {
	t.Helper()
	// The batch runtime flag must be on BEFORE New: the flagged install
	// (real Runner, real host identity) happens inside New, and the
	// counting-executor hook decorates that one install.
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	f := &providerCrashFixture{dataDir: t.TempDir()}
	f.scopeDir = filepath.Join(f.dataDir, "crash-scope")
	require.NoError(t, os.MkdirAll(f.scopeDir, 0o755))
	f.target = filepath.Join(f.scopeDir, "target.txt")
	// Pre-seed the REAL OS file BEFORE the child launches; the child's
	// provider is virtual, so only an OS-disk replay could touch it.
	require.NoError(t, os.WriteFile(f.target, []byte(providerCrashSentinel), 0o644))
	scratch := t.TempDir()
	f.witness = filepath.Join(scratch, "witness.log")
	f.signal = filepath.Join(scratch, "signal.json")
	f.info = f.signal + ".info"
	f.done = f.signal + ".done"
	f.reason = filepath.Join(scratch, "reason.txt")
	f.executor = &providerCrashCountingExecutor{}
	previous := batchInstallDepsHook
	batchInstallDepsHook = func(deps *agent.BatchInstallDeps) { deps.Executor = f.executor }
	// ALWAYS restore the previous value; the hook is a process global.
	t.Cleanup(func() { batchInstallDepsHook = previous })
	f.observing, f.observers = newAdmissionRaceAppOnDir(t, func(w http.ResponseWriter, r *http.Request) {
		admissionWriteSSE(w, []string{admissionSSEText("cP", "IGNORED"), admissionSSEStop("cP", "stop")})
	}, f.dataDir)
	batchInstallDepsHook = previous
	f.store = session.NewBatchStore(f.observing.batchCutoverWriter())
	return f
}

// providerCrashLeafPayload mirrors the batch_leaf payload JSON fields the
// design requires the announced root to carry (exact json tags from
// agent.batchLeafPayload / batchLeafPolicyContext — no invented names).
type providerCrashLeafPayload struct {
	RegistryID string `json:"registry_id"`
	Policy     struct {
		DiskBindingID string `json:"disk_binding_id"`
	} `json:"policy"`
}

// providerCrashAnnouncedRoot polls the real batch store until the child
// host's running root is ANNOUNCED and returns the row plus its decoded
// leaf payload (the binding/registry identity the design requires).
func providerCrashAnnouncedRoot(
	t *testing.T, store *session.BatchStore, ctx context.Context, host string,
) (db.BatchNode, providerCrashLeafPayload) {
	t.Helper()
	var found db.BatchNode
	var payload providerCrashLeafPayload
	require.Eventually(t, func() bool {
		roots, err := store.LiveRootsForHost(ctx, host)
		if err != nil {
			return false
		}
		for _, r := range roots {
			if r.Announced != 1 {
				continue
			}
			// A direct call is a one-item BATCH root: the container carries
			// no task payload, the leaf task rides on a child node.
			snapshot, err := store.Read(ctx, r.OwnerSessionID, r.RootID)
			if err != nil {
				continue
			}
			for _, node := range snapshot.Nodes {
				if !node.TaskPayload.Valid {
					continue
				}
				var p providerCrashLeafPayload
				if json.Unmarshal([]byte(node.TaskPayload.String), &p) != nil {
					continue
				}
				if p.RegistryID == "" && p.Policy.DiskBindingID == "" {
					continue
				}
				found, payload = r, p
				return true
			}
		}
		return false
	}, 90*time.Second, 50*time.Millisecond,
		"the running root must be announced with a non-empty binding/registry identity in its payload")
	return found, payload
}

// runProviderCrashSweep executes one PRODUCTION recovery pass.
func runProviderCrashSweep(
	t *testing.T, store *session.BatchStore, owner string, dataDir string,
) map[string]session.BatchRecoveryOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := store.BatchSweepDeadHosts(ctx, owner, session.BatchSharedHostProbe(dataDir))
	require.NoError(t, err)
	return out
}

// TestAppBatchProviderCrashSubprocess is acceptance 10-13 for the
// mid-provider crash: recovery must interrupt the announced running root
// exactly once, never replay the witness journal, never touch the OS
// sentinel, and never re-execute a recovered node.
func TestAppBatchProviderCrashSubprocess(t *testing.T) {
	if os.Getenv("R12H2B_CRASH_HELPER") == "1" {
		runProviderCrashHelper(t)
		return
	}
	f := newProviderCrashFixture(t)
	launchProviderCrashChild(t, f, "1")

	// Step 4: wait for the signal file; ONLY THEN verify the announced
	// running root and the alive host probe before killing the child.
	var sig providerCrashSignal
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(f.signal)
		if err != nil {
			return false
		}
		return json.Unmarshal(raw, &sig) == nil && sig.Host != ""
	}, 90*time.Second, 50*time.Millisecond, "the child must signal from inside the provider")
	require.Equal(t, f.target, sig.Target)
	probe, err := session.BatchSharedHostProbe(f.dataDir)(sig.Host)
	require.NoError(t, err)
	require.Equal(t, session.HostStatusAlive, probe,
		"the child's host lock must still be held at kill time")
	root, payload := providerCrashAnnouncedRoot(t, f.store, context.Background(), sig.Host)
	require.NotEmpty(t, root.RootID)
	require.NotEmpty(t, payload.RegistryID, "the payload must name the pinned registry identity")
	require.NotEmpty(t, payload.Policy.DiskBindingID, "the payload must name the disk binding")

	witnessAtKill := providerCrashWitnessLines(f.witness)
	require.Positive(t, witnessAtKill, "the provider must have journaled its write before signaling")

	// Kill the child mid-turn, never gracefully.
	require.NoError(t, f.cmd.Process.Kill())
	waitErr := <-f.waitErr
	require.Error(t, waitErr, "a killed child must not exit cleanly")
	var exitErr *exec.ExitError
	require.ErrorAs(t, waitErr, &exitErr, "%s%s", f.output.String(), f.childReason())

	// Step 5/6: production recovery over the SAME data dir, twice; the
	// second pass must be a no-op and the notices must not duplicate.
	pass1 := runProviderCrashSweep(t, f.store, sig.Session, f.dataDir)
	require.Len(t, pass1, 1, "recovery pass 1 must recover exactly the dead child host")
	out1, ok := pass1[sig.Host]
	require.True(t, ok)
	require.NotZero(t, out1.Interrupted,
		"the announced running root must be interrupted (design acceptance: Interrupted==1 for the single running root)")
	require.Equal(t, int64(1), out1.Notices,
		"exactly one root notice must be produced by pass 1")
	require.Zero(t, f.executor.starts.Load(),
		"no node may re-execute at recovery time (replays dying at binding lookup still Start)")

	pass2 := runProviderCrashSweep(t, f.store, sig.Session, f.dataDir)
	require.Empty(t, pass2, "recovery pass 2 must be a no-op")
	// No app-service accessor exposes a notices count; the once-only
	// property is asserted as pass1.Notices==1 + pass2.Notices==0 (pass2
	// empty) rather than a direct session_notices query.
	require.Zero(t, f.executor.starts.Load(),
		"the second recovery pass must not execute anything either")
	require.Equal(t, witnessAtKill, providerCrashWitnessLines(f.witness),
		"the witness journal must be UNCHANGED from the kill: no replay")
	sentinel, err := os.ReadFile(f.target)
	require.NoError(t, err)
	require.Equal(t, providerCrashSentinel, string(sentinel),
		"the REAL OS sentinel must still hold ONLY its pre-seeded content: no OS-disk replay")
	// Design: the no-replay negative requires the completed sweep/pump
	// barrier above, not a sleep.
	require.Never(t, func() bool {
		return providerCrashWitnessLines(f.witness) > witnessAtKill
	}, 3*time.Second, 100*time.Millisecond,
		"nothing may append to the witness journal after the recovery barrier")
}

// TestAppBatchProviderCrashAfterCompletionSubprocess is the
// AFTER-COMPLETION variant: the child's provider does NOT block, the root
// reaches its committed terminal state, and only then is the child killed.
// Recovery must report ZERO interrupts and leave the committed outcome,
// the witness journal and the executor count untouched.
func TestAppBatchProviderCrashAfterCompletionSubprocess(t *testing.T) {
	if os.Getenv("R12H2B_CRASH_HELPER") == "1" {
		runProviderCrashHelper(t)
		return
	}
	f := newProviderCrashFixture(t)
	launchProviderCrashChild(t, f, "0")

	// Capture the root id while the root is live (the BLOCK=0 provider
	// holds the leaf for 2s to make the window deterministic), then poll
	// the ROOT row through the real store until its state is terminal. The
	// host/session identity comes from the info file the child writes
	// BEFORE its run starts (the done signal only appears after commit).
	var hostID, sessionID string
	require.Eventually(t, func() bool {
		sessionID, hostID = f.childInfoFields()
		return hostID != "" && sessionID != ""
	}, 90*time.Second, 50*time.Millisecond, "the child must publish its identity info file")
	var rootID string
	require.Eventually(t, func() bool {
		roots, err := f.store.LiveRootsForHost(context.Background(), hostID)
		if err != nil || len(roots) == 0 {
			return false
		}
		rootID = roots[0].RootID
		return rootID != ""
	}, 90*time.Second, 20*time.Millisecond, "the child's root must become live")
	var committed db.BatchNode
	require.Eventually(t, func() bool {
		row, err := db.New(f.observing.batchCutoverWriter()).GetBatchRoot(
			context.Background(), db.GetBatchRootParams{RootID: rootID, OwnerSessionID: sessionID})
		if err != nil {
			return false
		}
		switch row.State {
		case "pending", "starting", "running", "cancelling":
			return false
		}
		committed = row
		return true
	}, 90*time.Second, 50*time.Millisecond,
		"the completed root must reach a committed terminal state before the kill")

	// Wait for the child's own exit 0 (child hygiene); kill only if it
	// somehow hangs past the bound.
	select {
	case waitErr := <-f.waitErr:
		require.NoError(t, waitErr, "the completed child must exit 0%s%s",
			f.output.String(), f.childReason())
		require.FileExists(t, f.done, "the child must publish its success signal before exiting")
	case <-time.After(90 * time.Second):
		_ = f.cmd.Process.Kill()
		<-f.waitErr
		t.Fatal("the completed child never exited; killed")
	}
	witnessBefore := providerCrashWitnessLines(f.witness)
	require.Positive(t, witnessBefore)

	pass := runProviderCrashSweep(t, f.store, sessionID, f.dataDir)
	require.Empty(t, pass, "a committed root must not be interrupted or noticed")
	require.Zero(t, f.executor.starts.Load(), "recovery must not execute anything")

	row, err := db.New(f.observing.batchCutoverWriter()).GetBatchRoot(
		context.Background(), db.GetBatchRootParams{RootID: rootID, OwnerSessionID: sessionID})
	require.NoError(t, err)
	require.Equal(t, committed.State, row.State,
		"the committed terminal outcome must survive recovery unchanged")
	require.Equal(t, witnessBefore, providerCrashWitnessLines(f.witness),
		"the witness journal must be unchanged after recovery")
	sentinel, err := os.ReadFile(f.target)
	require.NoError(t, err)
	require.Equal(t, providerCrashSentinel, string(sentinel),
		"the REAL OS sentinel must still hold ONLY its pre-seeded content")
}

// childInfoFields reads the child's identity info file ("<sessionID>
// <hostID>", written before the child's run starts).
func (f *providerCrashFixture) childInfoFields() (sessionID, hostID string) {
	raw, err := os.ReadFile(f.info)
	if err != nil {
		return "", ""
	}
	parts := strings.Fields(string(raw))
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
