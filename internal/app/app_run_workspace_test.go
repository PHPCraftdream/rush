// WS-1 workspace ownership on the run path (#1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md §1): which sessions THIS process
// may drive, and what happens when an operator asks it to drive somebody
// else's. Every test here runs against a real DB and a real session service,
// because what is under test is the interaction of app.New's wiring (the
// workspace it binds into the service) with the ownership check in
// resolveSession -- a mock service would only prove a branch was taken.
package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// wsGitRepo is a real git repository, so config.WorkspaceRoot has something to
// find: an isolated stand-in for one checkout of a project (mirrors the
// git-init pattern of internal/config/load_worktree_detect_test.go). A second
// call is a different, unrelated checkout.
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

// wsSessionLockFor lists the lock files under dataDir that belong to sessionID,
// so a test can prove the refusal never reached the inter-process lock.
func wsSessionLockFor(dataDir, sessionID string) []string {
	entries, err := os.ReadDir(filepath.Join(dataDir, "locks"))
	if err != nil {
		return nil // no locks dir: no lock was ever taken
	}
	var hits []string
	for _, e := range entries {
		if !e.IsDir() && strings.Contains(e.Name(), sessionID) {
			hits = append(hits, e.Name())
		}
	}
	return hits
}

// wsNewApp builds a minimal App over its own data dir: enough of New for
// resolveSession and the run path, without a provider to complete a turn.
func wsNewApp(t *testing.T, workingDir string) (*App, string) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := config.Init(workingDir, dataDir, false)
	require.NoError(t, err)
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	return application, dataDir
}

// TestResolveSession_ForeignSessionIsRefusedBeforeAnyWrite: a `--session`
// naming a session bound to ANOTHER workspace is a typed refusal -- neither a
// silent run against that checkout's paths, nor an "already in use" raised by
// the agent that checkout is running.
//
// The refusal must land before the inter-process session lock is taken and
// before any write to the row: the assertions after the error pin all of it
// (unchanged row, no lock file).
//
// Revert-check: deleting the `if !app.owns(sess)` block in resolveSession
// (internal/app/app_run_session.go) fails this test at require.ErrorIs below
// -- the foreign session is returned and the turn would run.
func TestResolveSession_ForeignSessionIsRefusedBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	application, dataDir := wsNewApp(t, t.TempDir()) // home process: no git repo
	q := db.New(application.DB())

	// A session this app's own service did NOT create, bound to another
	// checkout. A home process refuses it just as a linked one does.
	foreignRoot := wsGitRepo(t)
	foreignSvc := session.NewServiceWithWorkspace(q, application.DB(), nil, nil, foreignRoot, foreignRoot, false)
	foreign, err := foreignSvc.Create(ctx, "foreign session")
	require.NoError(t, err)

	before, err := q.GetSessionByID(ctx, foreign.ID)
	require.NoError(t, err)

	_, err = application.resolveSession(ctx, foreign.ID, false)
	require.Error(t, err)
	require.ErrorIs(t, err, session.ErrForeignWorkspace,
		"the refusal must stay typed: the run-queue pump nacks a foreign row without penalty on it")
	require.Contains(t, err.Error(), foreign.WorkspaceRoot, "the message names the owning workspace")
	require.Contains(t, err.Error(), foreign.GitBranch, "the message names that checkout's branch")
	require.Contains(t, err.Error(), "rush sessions fork", "the message offers the sanctioned way forward")
	require.NotContains(t, err.Error(), "workspace no longer exists",
		"the owning checkout still exists, so nothing says it does not")

	after, err := q.GetSessionByID(ctx, foreign.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused --session must not touch the row")
	require.Empty(t, wsSessionLockFor(dataDir, foreign.ID),
		"the refusal must land before the inter-process session lock")
}

// TestResolveSession_ForeignWorkspaceGoneIsReportedAsSuch: when the owning
// checkout no longer exists (`git worktree remove`), the refusal says so --
// otherwise the operator hunts for a directory that is not there.
//
// Revert-check: dropping the os.Stat branch in agent.ForeignWorkspaceError
// fails the assertion below.
func TestResolveSession_ForeignWorkspaceGoneIsReportedAsSuch(t *testing.T) {
	ctx := context.Background()
	application, _ := wsNewApp(t, t.TempDir())
	q := db.New(application.DB())

	gone := filepath.Join(t.TempDir(), "removed-worktree")
	goneSvc := session.NewServiceWithWorkspace(q, application.DB(), nil, nil, gone, "", false)
	foreign, err := goneSvc.Create(ctx, "session of a removed checkout")
	require.NoError(t, err)

	_, err = application.resolveSession(ctx, foreign.ID, false)
	require.ErrorIs(t, err, session.ErrForeignWorkspace)
	require.Contains(t, err.Error(), "(workspace no longer exists)")
	require.Contains(t, err.Error(), "rush sessions fork")
}

// TestResolveSession_LegacyRowIsRefusedFromLinkedAndDrivenFromHome: the WS-1
// predicate's legacy half, from all three sides. A row with no binding at all
// (”) belongs to the owner of the data directory. Today every process owns
// its data directory (config.WorkspaceHome() == true until SD-D #1143), so a
// git-checkout process with a NON-EMPTY workspace drives the legacy row too;
// only the shared-from-linked process SD-D will introduce (home=false)
// refuses it, because in a shared database it cannot tell that row from one
// its own checkout's pre-WS-1 binary created.
//
// Revert-check: dropping the ownership check in resolveSession makes the
// shared-from-linked sub-case stop refusing. Mutant B (the home regression):
// in owns() (internal/app/app_run_session.go) restore
// `app.workspaceRoot == ""` as the home argument -- the git-checkout
// sub-test fails at require.NoError ("session X belongs to ...").
func TestResolveSession_LegacyRowIsRefusedFromLinkedAndDrivenFromHome(t *testing.T) {
	ctx := context.Background()
	application, _ := wsNewApp(t, t.TempDir())
	q := db.New(application.DB())

	// A legacy row: written exactly the way every pre-WS-1 binary wrote it.
	legacy, err := session.NewService(q, application.DB()).Create(ctx, "legacy session")
	require.NoError(t, err)
	require.Empty(t, legacy.WorkspaceRoot)

	t.Run("home process drives the legacy row", func(t *testing.T) {
		sess, err := application.resolveSession(ctx, legacy.ID, false)
		require.NoError(t, err)
		require.Equal(t, legacy.ID, sess.ID)
	})

	t.Run("today's git-checkout process drives the legacy row", func(t *testing.T) {
		// Non-empty workspace, home unchanged (true): the pre-SD-D world.
		application.workspaceRoot = wsGitRepo(t)
		t.Cleanup(func() { application.workspaceRoot = "" })
		sess, err := application.resolveSession(ctx, legacy.ID, false)
		require.NoError(t, err,
			"a git checkout is a home process until SD-D: its legacy history stays drivable")
		require.Equal(t, legacy.ID, sess.ID)
	})

	t.Run("shared-from-linked process refuses the legacy row", func(t *testing.T) {
		application.workspaceRoot = wsGitRepo(t)
		application.home = false
		t.Cleanup(func() { application.workspaceRoot = ""; application.home = true })
		_, err := application.resolveSession(ctx, legacy.ID, false)
		require.ErrorIs(t, err, session.ErrForeignWorkspace)
	})
}

// TestResolveSession_UseLastContinuesLegacyFromGitCheckout is the
// --continue twin of the git-checkout sub-test above (#1142 step C's home
// regression): GetLast is workspace-filtered in SQL, and its home argument
// comes from the app wiring -- a git-checkout process (non-empty root,
// home=true) must still find the legacy row it created before WS-1.
//
// Revert-check (mutant B, GetLast half): pass `config.WorkspaceRoot(dir) == ""`
// -- i.e. any root-derived home -- into NewServiceWithWorkspace in app.New;
// resolveSession then errors "no sessions found" at require.NoError below.
func TestResolveSession_UseLastContinuesLegacyFromGitCheckout(t *testing.T) {
	ctx := context.Background()
	repo := wsGitRepo(t)
	application, _ := wsNewApp(t, repo)
	require.NotEmpty(t, application.workspaceRoot, "fixture: a real checkout has a non-empty root")
	require.True(t, application.ProcessHome(), "fixture: every process is home until SD-D")

	// A legacy row: written by a home service over this app's DB -- the
	// pre-WS-1 shape (workspace_root '') this git-checkout process owned
	// before WS-1 existed. Creating it via application.Sessions would bind
	// it to the repo root and make the test blind to the home argument.
	legacySvc := session.NewService(db.New(application.DB()), application.DB())
	_, err := legacySvc.Create(ctx, "pre-WS-1 history")
	require.NoError(t, err)

	sess, err := application.resolveSession(ctx, "", true)
	require.NoError(t, err, "--continue must find the legacy row from its git-checkout owner")
	require.NotEmpty(t, sess.ID)
}

// TestResolveSession_OwnSessionIsDrivenFromItsOwnWorkspace: the symmetric
// case -- the row's workspace IS this process's, so it is driven exactly as
// before WS-1. Together with the table above this is §1's whole
// "X свой: как сейчас" row.
//
// Revert-check: inverting the predicate (refusing same-workspace rows) fails
// this test.
func TestResolveSession_OwnSessionIsDrivenFromItsOwnWorkspace(t *testing.T) {
	ctx := context.Background()
	own := wsGitRepo(t)
	application, _ := wsNewApp(t, t.TempDir())
	q := db.New(application.DB())

	ownSvc := session.NewServiceWithWorkspace(q, application.DB(), nil, nil, own, own, false)
	sess, err := ownSvc.Create(ctx, "own session")
	require.NoError(t, err)
	require.Equal(t, own, sess.WorkspaceRoot, "the service must bind what it creates")

	application.workspaceRoot = own
	t.Cleanup(func() { application.workspaceRoot = "" })
	got, err := application.resolveSession(ctx, sess.ID, false)
	require.NoError(t, err)
	require.Equal(t, sess.ID, got.ID)
}

// TestNew_BindsWorkspaceRootToTheConfigStoreWorkingDir: app.New derives the
// process's workspace from ConfigStore.WorkingDir() -- NOT os.Getwd(), which
// differs for an SDK host -- and the session service it builds binds every row
// it creates to that root.
//
// Revert-check: replacing store.WorkingDir() with os.Getwd() in app.New fails
// the last assertion whenever the process CWD is outside the store's working
// dir; passing "" instead of workspaceRoot to NewServiceWithWorkspace fails
// the first.
func TestNew_BindsWorkspaceRootToTheConfigStoreWorkingDir(t *testing.T) {
	ctx := context.Background()
	repo := wsGitRepo(t)
	application, _ := wsNewApp(t, repo)

	require.Equal(t, config.WorkspaceRoot(repo), application.workspaceRoot,
		"New must derive the workspace from the config store's working dir")
	require.NotEmpty(t, application.workspaceRoot, "a real checkout resolves to a non-empty workspace root (its home flag is separate: true until SD-D)")

	// The differential that rules out os.Getwd(): under `go test` the process
	// CWD is this package's own directory, a DIFFERENT checkout.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NotEqual(t, config.WorkspaceRoot(cwd), application.workspaceRoot,
		"the workspace must not come from os.Getwd()")

	sess, err := application.Sessions.Create(ctx, "bound row")
	require.NoError(t, err)
	require.Equal(t, application.workspaceRoot, sess.WorkspaceRoot,
		"every row this process creates carries its workspace")
	require.NotEmpty(t, sess.GitBranch, "the branch is read from <git-dir>/HEAD, not by spawning git")
}

// TestResolveSession_UseLastOnlySeesOwnSessions: `--continue` cannot land on
// another workspace's session. The filter is the one GetLast carries in SQL
// (step 1); this pins that the app-level wiring keeps feeding it, i.e. that
// New hands the service its real workspace rather than a blank one.
//
// Revert-check: constructing the session service with an empty workspace root
// in app.New makes the foreign row the continuation target here.
func TestResolveSession_UseLastOnlySeesOwnSessions(t *testing.T) {
	ctx := context.Background()
	application, _ := wsNewApp(t, t.TempDir())
	q := db.New(application.DB())

	foreign := wsGitRepo(t)
	foreignSvc := session.NewServiceWithWorkspace(q, application.DB(), nil, nil, foreign, foreign, false)
	_, err := foreignSvc.Create(ctx, "somebody else's session")
	require.NoError(t, err)

	application.workspaceRoot = wsGitRepo(t)
	t.Cleanup(func() { application.workspaceRoot = "" })

	_, err = application.resolveSession(ctx, "", true)
	require.Error(t, err, "no own session exists: the foreign one is not a continuation target")
	require.Contains(t, err.Error(), "no sessions found")
}

// TestRunNonInteractive_ForeignSessionIsRefusedEndToEnd: the whole CLI path.
// A `rush run --session <id>` for a foreign row stops at the ownership check --
// before the provider is called, before the lock, and before the first
// assistant message.
//
// Revert-check: removing the ownership check from resolveSession makes this
// test fail at the provider handler's t.Error (the turn runs) and at the
// ErrorIs below.
func TestRunNonInteractive_ForeignSessionIsRefusedEndToEnd(t *testing.T) {
	application, sessionID, dataDir := newLockBusyCLITestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("provider must never be called: the foreign-session refusal must be caught before any turn")
	})
	ctx := context.Background()

	// Re-bind the harness's own session to another checkout: the row now
	// belongs somewhere this process is not.
	foreignRoot := wsGitRepo(t)
	_, err := application.DB().ExecContext(ctx,
		`UPDATE sessions SET workspace_root = ?, git_branch = 'other-branch' WHERE id = ?`,
		foreignRoot, sessionID)
	require.NoError(t, err)

	before, err := application.Sessions.Get(ctx, sessionID)
	require.NoError(t, err)

	_, err = application.RunNonInteractiveWithResult(ctx, io.Discard, "hello",
		RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.Error(t, err)
	require.ErrorIs(t, err, session.ErrForeignWorkspace)
	require.Contains(t, err.Error(), foreignRoot)
	require.Contains(t, err.Error(), "rush sessions fork")

	after, err := application.Sessions.Get(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, before, after, "the refused run must not write the session")
	require.Empty(t, wsSessionLockFor(dataDir, sessionID),
		"the refusal must land before the inter-process session lock")

	msgs, err := application.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotEqual(t, message.Assistant, m.Role,
			"no assistant message may be written for a session this process may not drive")
	}
}
