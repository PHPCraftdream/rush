// The WS-1 backstop in sessionAgent.runOwned (#1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md §1 "любой ход (страховка)"): no
// turn may be driven for a session that belongs to another workspace, no
// matter which entry point reached it.
//
// The tests are shaped like run_lock_test.go's -- a bare *sessionAgent with
// just enough state for Run to reach the guard -- because the guard sits
// between the inter-process lock and the turn preamble, so no provider is
// ever consulted. The "gets past the guard" cases reuse run_lock_test.go's
// blocking-Get trick: its second Get blocks on a channel the test closes, so
// a blocked second Get means the guard let the call through -- deterministically,
// with no polling and no race.

package agent

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wsForeignSession is the id used by every test here; the row it names is
// supplied per-case by wsSessionService.
const wsForeignSession = "foreign-workspace-session"

// wsForeignRoot is the workspace the foreign rows belong to.
const wsForeignRoot = `D:\ws\foreign`

// wsGitRepo is a real git repository, so config.WorkspaceRoot has something to
// find (mirrors internal/config/load_worktree_detect_test.go's git-init
// pattern): an isolated stand-in for one checkout of a project.
func wsGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitInit := platform.Command(t.Context(), "git", "init", "-q")
	gitInit.Dir = repo
	require.NoError(t, gitInit.Run())
	return repo
}

// wsSessionService is a session.Service fake whose Get answers the FIRST call
// with row/err -- that call is the guard's -- and blocks every later call (the
// turn preamble's, reached only once the guard is behind the call) until the
// test releases it. It is run_lock_test.go's blockingGetSessionService pattern,
// generalised to two answers.
//
// Embedded interface + the nil default means an unexpected method call panics
// loudly rather than silently succeeding; nothing besides Get is reachable
// before the guard decides.
type wsSessionService struct {
	session.Service
	row session.Session
	err error
	// gets counts the calls, so a test can prove the guard read the row
	// exactly once and nothing after it ran.
	gets int
	// block2 blocks every Get after the first; release2 unblocks it.
	block2   chan struct{}
	release2 chan struct{}
}

func newWsSessionService(row session.Session, err error) *wsSessionService {
	return &wsSessionService{
		row:      row,
		err:      err,
		block2:   make(chan struct{}),
		release2: make(chan struct{}),
	}
}

func (w *wsSessionService) Get(context.Context, string) (session.Session, error) {
	w.gets++
	if w.gets > 1 {
		// Past the guard: park here until the test decides what happens next,
		// so it can prove (without polling) that the guard let the call run.
		close(w.block2)
		<-w.release2
		return session.Session{}, errors.New("wsSessionService: released by the test")
	}
	return w.row, w.err
}

// newWsGuardAgent is the same minimal agent run_lock_test.go builds, plus a
// session service for the guard to consult.
func newWsGuardAgent(dataDir string, sessions session.Service) *sessionAgent {
	a := newLockTestSessionAgent(dataDir, false /* isSubAgent */)
	a.sessions = sessions
	return a
}

// TestRun_ForeignSessionIsRefusedBeforeAnyTurnAndLeavesNoLock: Run on a
// session belonging to another workspace must return the typed refusal before
// any assistant message, before the turn preamble and without leaving a
// session lock behind for the real owner in the other checkout to trip over.
//
// The guard is taken AFTER the inter-process lock (the ordering tests
// run_lock_test.go:197 and run_preamble_timeout_test.go:74 pin Run's shape as
// "lock first, then the preamble's own a.sessions.Get"), so what proves "no
// side effects" is the state the refusal LEAVES behind: the lock Run briefly
// took is already released, the row was read exactly once, and nothing reached
// the preamble. An earlier revision of this test asserted the refusal happened
// BEFORE the lock by pre-holding the lock itself; that can no longer be
// asserted, and this is the assertion that still carries the guarantee.
//
// Revert-check: deleting the refuseForeignSession guard block in runOwned
// (internal/agent/agent_run.go) makes this test fail at require.ErrorIs --
// the call proceeds past the guard into the turn preamble.
func TestRun_ForeignSessionIsRefusedBeforeAnyTurnAndLeavesNoLock(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()

	// No lock is pre-held here: the guard sits behind Run's own lock
	// acquisition now, so a holder standing in for the owning process would
	// only make this case exercise the lock-busy rejection ("already in use")
	// instead of the WS-1 refusal it is about.

	svc := newWsSessionService(session.Session{
		ID:            wsForeignSession,
		WorkspaceRoot: wsForeignRoot,
		GitBranch:     "feature-x",
	}, nil)
	a := newWsGuardAgent(dataDir, svc)

	result, runErr := a.Run(context.Background(), SessionAgentCall{
		SessionID: wsForeignSession,
		Prompt:    "test prompt",
	})
	require.Error(t, runErr, "Run must refuse a session this process does not own")
	assert.Nil(t, result)
	require.ErrorIs(t, runErr, session.ErrForeignWorkspace,
		"the refusal must stay typed: the pump nacks a foreign row without penalty on it")
	assert.Contains(t, runErr.Error(), wsForeignRoot, "the message names the owning workspace")
	assert.Contains(t, runErr.Error(), "feature-x", "the message names that checkout's branch")
	assert.Contains(t, runErr.Error(), "rush sessions fork", "the message offers the sanctioned way forward")

	// The guard consulted the row exactly once, and never got as far as the
	// turn preamble's own Get.
	require.Equal(t, 1, svc.gets, "the guard must be the only session read the refusal needs")
	select {
	case <-svc.block2:
		t.Fatal("Run reached the turn preamble: the guard did not stop it")
	default:
	}

	// The refusal took the inter-process lock down with it (lk.Release +
	// the deferred abandonOwnershipWithHandoff, exactly as a turn would), so
	// the session's real owner never finds a stale lock file. This is what
	// "no side effects" means now that the guard runs behind the lock.
	lk, lockErr := session.TryAcquireSessionLock(dataDir, wsForeignSession)
	require.NoError(t, lockErr, "a refused call must leave no session lock behind: %v", lockErr)
	require.NoError(t, lk.Release())
}

// TestRun_ForeignSessionIsRefusedOnEveryRunEntryPoint: the guard lives in
// runOwned, which RunWithReservedOwnership also funnels into, so the
// web/interrupt-style path (an ownership era the caller claimed before Run)
// gets the same refusal.
//
// Revert-check: moving the guard out of runOwned into Run only makes this test
// fail at require.ErrorIs.
func TestRun_ForeignSessionIsRefusedOnEveryRunEntryPoint(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	svc := newWsSessionService(session.Session{
		ID:            wsForeignSession,
		WorkspaceRoot: wsForeignRoot,
		GitBranch:     "feature-x",
	}, nil)
	a := newWsGuardAgent(dataDir, svc)

	// The pre-reserved era ExecuteRun claims for fail-fast (SDK) callers
	// before reaching RunWithReservedOwnership.
	holdCtx, epoch, cancel, ok := a.ReserveExclusive(context.Background(), wsForeignSession)
	require.True(t, ok)
	t.Cleanup(func() { a.ReleaseExclusive(wsForeignSession, epoch, cancel) })
	_ = holdCtx

	result, runErr := a.RunWithReservedOwnership(context.Background(), SessionAgentCall{
		SessionID: wsForeignSession,
		Prompt:    "test prompt",
	}, epoch, cancel, nil)
	require.Error(t, runErr)
	assert.Nil(t, result)
	require.ErrorIs(t, runErr, session.ErrForeignWorkspace)
	require.Equal(t, 1, svc.gets)
	select {
	case <-svc.block2:
		t.Fatal("Run reached the turn preamble: the guard did not stop it")
	default:
	}
}

// TestRun_LinkedProcessRefusesForeignAndLegacyRows: the guard's workspace
// comes from the config store and its home flag from the single source
// config.WorkspaceHome(). Today that flag is true everywhere (until SD-D
// #1143), so an agent whose config store names a real checkout is a HOME
// process with a non-empty workspace: a foreign row is refused, a legacy
// unbound row is DRIVEN (it is this checkout's pre-WS-1 history), and a row
// bound to its own workspace is driven. The genuinely shared-from-linked
// process (home=false) -- which SD-D will introduce and wsHomeOverride models
// here -- refuses the legacy row too.
//
// Revert-check (mutant D, the home regression): in processWorkspace
// (internal/agent/agent_run.go) restore `return root, root == ""` (home from
// the root) -- the "legacy row is driven by today's git-checkout" sub-test
// fails at require.NotErrorIs (the legacy row comes back refused as if this
// were shared-from-linked); the override sub-test keeps passing only because
// it sets home explicitly.
func TestRun_LinkedProcessRefusesForeignAndLegacyRows(t *testing.T) {
	own := wsGitRepo(t)
	store, err := config.Init(own, t.TempDir(), false)
	require.NoError(t, err)
	// The value the guard derives from the store, which is also what a session
	// created in this checkout carries (the session service binds
	// config.WorkspaceRoot of the same working dir).
	ownRoot := config.WorkspaceRoot(own)
	require.NotEmpty(t, ownRoot, "the fixture's checkout must resolve to a workspace")

	dataDir := t.TempDir()

	sharedFromLinked := false

	t.Run("foreign row is refused", func(t *testing.T) {
		svc := newWsSessionService(session.Session{
			ID:            wsForeignSession,
			WorkspaceRoot: wsForeignRoot,
			GitBranch:     "feature-x",
		}, nil)
		a := newWsGuardAgent(dataDir, svc)
		a.config = store

		_, runErr := a.Run(context.Background(), SessionAgentCall{SessionID: wsForeignSession, Prompt: "p"})
		require.ErrorIs(t, runErr, session.ErrForeignWorkspace)
		assert.Contains(t, runErr.Error(), wsForeignRoot)
	})

	t.Run("legacy row is driven by today's git-checkout process", func(t *testing.T) {
		// No override: home comes from config.WorkspaceHome() == true, so the
		// legacy row (every pre-WS-1 row's shape) stays drivable.
		svc := newWsSessionService(session.Session{ID: wsForeignSession}, nil)
		a := newWsGuardAgent(dataDir, svc)
		a.config = store

		runErrCh := make(chan error, 1)
		go func() {
			_, err := a.Run(context.Background(), SessionAgentCall{SessionID: wsForeignSession, Prompt: "p"})
			runErrCh <- err
		}()

		// Blocked inside the turn preamble's Get: the guard let this call
		// through, which is the whole point -- a legacy row is driven.
		select {
		case <-svc.block2:
		case runErr := <-runErrCh:
			t.Fatalf("Run refused the legacy row (err=%v) -- it is this checkout's history", runErr)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Run to reach the preamble's Get")
		}
		close(svc.release2)

		select {
		case runErr := <-runErrCh:
			require.Error(t, runErr)
			assert.NotErrorIs(t, runErr, session.ErrForeignWorkspace,
				"a git checkout is a home process until SD-D: its legacy history stays drivable")
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for Run to return")
		}
	})

	t.Run("legacy row is refused by a shared-from-linked process", func(t *testing.T) {
		svc := newWsSessionService(session.Session{ID: wsForeignSession}, nil)
		a := newWsGuardAgent(dataDir, svc)
		a.config = store
		a.wsHomeOverride = &sharedFromLinked

		_, runErr := a.Run(context.Background(), SessionAgentCall{SessionID: wsForeignSession, Prompt: "p"})
		require.ErrorIs(t, runErr, session.ErrForeignWorkspace,
			"a shared-from-linked process must not drive a legacy unbound row")
	})

	t.Run("own row is driven", func(t *testing.T) {
		svc := newWsSessionService(session.Session{
			ID:            wsForeignSession,
			WorkspaceRoot: ownRoot,
			GitBranch:     "main",
		}, nil)
		a := newWsGuardAgent(dataDir, svc)
		a.config = store

		runErrCh := make(chan error, 1)
		go func() {
			_, err := a.Run(context.Background(), SessionAgentCall{SessionID: wsForeignSession, Prompt: "p"})
			runErrCh <- err
		}()

		// Blocked inside the turn preamble's Get: the guard let this call
		// through, so it is demonstrably past the guard AND the inter-process
		// lock -- and therefore holding that lock.
		select {
		case <-svc.block2:
		case runErr := <-runErrCh:
			t.Fatalf("Run returned before the preamble (err=%v): was it refused by the guard?", runErr)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Run to reach the preamble's Get")
		}
		_, lockErr := session.TryAcquireSessionLock(dataDir, wsForeignSession)
		var busyErr *session.SessionLockBusyError
		require.Error(t, lockErr)
		require.True(t, errors.As(lockErr, &busyErr),
			"the run that got past the guard holds the session lock: got %v", lockErr)

		close(svc.release2)
		select {
		case runErr := <-runErrCh:
			require.Error(t, runErr)
			assert.NotErrorIs(t, runErr, session.ErrForeignWorkspace,
				"a row bound to this process's workspace must not be refused")
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for Run to return")
		}
	})
}

// TestRun_LegacySessionIsDrivenPastTheGuard: the backstop must not refuse what
// this process OWNS. An agent with no config (every bare fixture) is a home
// process, so a legacy unbound row stays drivable -- the whole point of the
// home branch of Owns.
//
// Revert-check: making the guard refuse legacy rows (dropping the home
// argument from its Owns call) makes this test fail at require.NotErrorIs.
func TestRun_LegacySessionIsDrivenPastTheGuard(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	svc := newWsSessionService(session.Session{ID: wsForeignSession}, nil)
	a := newWsGuardAgent(dataDir, svc)

	runErrCh := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), SessionAgentCall{
			SessionID: wsForeignSession,
			Prompt:    "test prompt",
		})
		runErrCh <- err
	}()

	select {
	case <-svc.block2:
	case runErr := <-runErrCh:
		t.Fatalf("Run returned before the preamble (err=%v): was it refused by the guard?", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to reach the preamble's Get")
	}

	// Blocked inside the preamble's Get, i.e. demonstrably past the guard and
	// past the lock -- and therefore holding that lock.
	_, lockErr := session.TryAcquireSessionLock(dataDir, wsForeignSession)
	var busyErr *session.SessionLockBusyError
	require.Error(t, lockErr)
	require.True(t, errors.As(lockErr, &busyErr),
		"the run that got past the guard holds the session lock: got %v", lockErr)

	close(svc.release2)
	select {
	case runErr := <-runErrCh:
		require.Error(t, runErr)
		assert.NotErrorIs(t, runErr, session.ErrForeignWorkspace,
			"a legacy row is owned by the home process and must not be refused")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Run to return")
	}
}

// TestRun_UnreadableSessionIsNotAWorkspaceRefusal: a session that cannot be
// read at all (deleted mid-flight, an unreadable row) is not a workspace
// question. Refusing there would turn every "session gone" race into a typed
// WS-1 error, so the guard steps aside and the historical behaviour stands.
//
// Revert-check: returning the refusal on a Get error makes this test fail at
// require.NotErrorIs and at the "belongs to" assertion.
func TestRun_UnreadableSessionIsNotAWorkspaceRefusal(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	svc := newWsSessionService(session.Session{}, errors.New("sql: no rows in result set"))
	a := newWsGuardAgent(dataDir, svc)

	runErrCh := make(chan error, 1)
	go func() {
		_, err := a.Run(context.Background(), SessionAgentCall{
			SessionID: "gone-session",
			Prompt:    "test prompt",
		})
		runErrCh <- err
	}()

	select {
	case <-svc.block2:
	case runErr := <-runErrCh:
		t.Fatalf("Run returned before the preamble (err=%v): an unreadable row must not be a refusal", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Run to reach the preamble's Get")
	}
	close(svc.release2)
	select {
	case runErr := <-runErrCh:
		require.Error(t, runErr)
		assert.NotErrorIs(t, runErr, session.ErrForeignWorkspace,
			"an unreadable session is not a workspace refusal")
		assert.False(t, strings.Contains(runErr.Error(), "belongs to"),
			"an unreadable session must not produce the WS-1 message")
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Run to return")
	}
}
