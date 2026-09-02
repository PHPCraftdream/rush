package app

// T12 REGRESSION (P0): a folder-SCOPED queued call orphaned DURABLY by its
// owner's terminal failure must restart with its scope INTACT. The pump
// rebuilds the call from its persisted row; pre-fix, SessionAgentCallData
// carried no CallOptions and no Tools (both json:"-"), so the REBUILT call
// had neither — the restarted turn silently fell back to the session's
// SHARED, UNSCOPED toolset (view/write/edit/glob/grep/ls/bash/...) AND its
// fs_write was built with a ZERO scope that denies every item, so even
// IN-scope writes never landed. Two halves, two pins: (a) the toolset pin
// parses the REBUILT turn's request body — fs_read/fs_write/fs_list must be
// present, view/write/edit/... must be ABSENT (pre-fix the unscoped set was
// there); (b) the behavior pin requires the in-scope file to EXIST with the
// written content and the out-of-scope file to NOT exist (pre-fix the
// zero-scope fs_write denied even the in-scope item), plus the denial
// reason "outside every folder scope" fed back to the provider.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestExecuteRunDurablyOrphanedScopedCallRestartsScoped(t *testing.T) {
	var (
		gate      = make(chan struct{})
		started   = make(chan struct{})
		startOnce sync.Once

		// restartRounds counts restart provider rounds: exactly 2 for the
		// single rebuilt turn (the fs_write round + the follow-up round).
		// A 3rd means a denial failed to stop a turn or a round was lost.
		restartRounds atomic.Int64

		bodyMu     sync.Mutex
		round1Body string
		round2Body string
	)
	recordBody := func(which, body string) {
		bodyMu.Lock()
		defer bodyMu.Unlock()
		if which == "round1" {
			round1Body = body
		} else {
			round2Body = body
		}
	}
	snapshotBodies := func() (string, string) {
		bodyMu.Lock()
		defer bodyMu.Unlock()
		return round1Body, round2Body
	}

	scopedOverrides := RunOverrides{
		FolderScopes: []permission.FolderScopeEntry{{
			Dir: "scope-inside",
			Ops: []permission.FileOp{
				permission.FileOpCreate,
				permission.FileOpOverwrite,
				permission.FileOpRead,
				permission.FileOpList,
			},
		}},
	}

	handler := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), "tw1"):
			// Follow-up round of the REBUILT turn: the fs_write result
			// (one item granted, one denied) is in the messages; end the
			// turn with final text.
			restartRounds.Add(1)
			recordBody("round2", string(body))
			admissionWriteSSE(w, []string{
				admissionSSEText("c4", "RESTART_SCOPE_DONE"),
				admissionSSEStop("c4", "stop"),
			})
		case strings.Contains(string(body), "QUEUED_SCOPE_TURN_MARKER"):
			// Round 1 of the REBUILT turn: hand back ONE fs_write that
			// touches an IN-scope file and an OUT-of-scope file.
			restartRounds.Add(1)
			recordBody("round1", string(body))
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("c3", "tw1", "fs_write",
					`{"items":[{"path":"scope-inside/inside.txt","content":"INSIDE_OK"},{"path":"outside.txt","content":"OUTSIDE_MUST_NOT_LAND"}]}`),
				admissionSSEStop("c3", "tool_calls"),
			})
		case strings.Contains(string(body), "OWNER_SCOPE_TURN_MARKER"):
			// The owner: block mid-turn, then fail terminally so its
			// finalizer orphans the queued call into the durable queue.
			startOnce.Do(func() { close(started) })
			<-gate
			http.Error(w, "terminal provider failure", http.StatusBadRequest)
		default:
			admissionWriteSSE(w, []string{
				admissionSSEText("c0", "IGNORED"),
				admissionSSEStop("c0", "stop"),
			})
		}
	}

	application, sessionID := newAdmissionRaceApp(t, handler)
	require.NotNil(t, application.RunQueuePump,
		"this test drives the durable restart through the production pump; the app must have started one")
	workingDir := application.config.WorkingDir()

	outcomes := make(chan admissionOutcome, 2)

	// Owner turn: same folder scope, blocked mid-turn by the gate.
	start := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start, 1,
		"owner turn OWNER_SCOPE_TURN_MARKER hold", scopedOverrides, false)
	close(start)
	select {
	case <-started:
	case <-time.After(60 * time.Second):
		t.Fatal("owner call never reached the provider; cannot stage the mid-turn owner")
	}

	// Queued scoped call: durably orphaned when the owner fails.
	start2 := make(chan struct{})
	admissionLaunch(t, application, sessionID, outcomes, start2, 2,
		"queued scoped turn QUEUED_SCOPE_TURN_MARKER", scopedOverrides, false)
	close(start2)
	require.Eventually(t, func() bool {
		return application.AgentCoordinator.QueuedPrompts(sessionID) >= 1
	}, 10*time.Second, time.Millisecond, "the queued scoped call never reached the mailbox's submitted queue")

	select {
	case o := <-outcomes:
		require.Equal(t, 2, o.idx)
		require.NoError(t, o.err, "the queueing caller must return cleanly")
		require.NotNil(t, o.res)
	case <-time.After(60 * time.Second):
		t.Fatal("queued scoped call did not return")
	}

	// Fail the owner terminally; the finalizer must orphan the queued
	// call into the durable run queue and the pump must rebuild it.
	close(gate)
	select {
	case o := <-outcomes:
		require.Equal(t, 1, o.idx)
		require.Error(t, o.err, "the owner's turn must fail on the terminal provider error")
	case <-time.After(60 * time.Second):
		t.Fatal("owner call did not return after the gate opened")
	}

	// Wait for the pump-driven restart to complete BOTH rounds and for
	// the durable row to be acked.
	require.Eventually(t, func() bool {
		if restartRounds.Load() < 2 {
			return false
		}
		r1, r2 := snapshotBodies()
		return r1 != "" && r2 != ""
	}, 120*time.Second, 10*time.Millisecond, "the pump never rebuilt the orphaned scoped call into two provider rounds")
	require.Eventually(t, func() bool {
		has, err := application.Sessions.HasOutstandingRunQueueEntriesForSession(context.Background(), sessionID)
		return err == nil && !has
	}, 120*time.Second, 10*time.Millisecond, "the durable run-queue row was never acked after the restart")

	// (a) EXACTLY two restart rounds.
	require.Equal(t, int64(2), restartRounds.Load(),
		"the rebuilt turn must make exactly two provider rounds; a third means a denial failed to stop or a round was lost")

	r1, r2 := snapshotBodies()
	require.NotEmpty(t, r1)
	require.NotEmpty(t, r2)

	// (b) TOOLSET PIN: the REBUILT turn's request must carry the SCOPED
	// toolset — never the shared unscoped set.
	for _, present := range []string{`"name":"fs_write"`, `"name":"fs_read"`, `"name":"fs_list"`} {
		require.Contains(t, r1, present,
			"the durably restarted scoped call's toolset must contain %s", present)
	}
	for _, absent := range []string{
		`"name":"view"`, `"name":"write"`, `"name":"edit"`, `"name":"multiedit"`,
		`"name":"glob"`, `"name":"grep"`, `"name":"ls"`, `"name":"download"`,
		`"name":"git_read"`, `"name":"bash"`, `"name":"fs_delete"`,
		`"name":"fs_replace"`, `"name":"fs_write_lines"`,
	} {
		require.NotContains(t, r1, absent,
			"a durably-restarted scoped call came back with the unscoped toolset: it contains %s (the T12 restart promotion)", absent)
	}

	// (c) BEHAVIOR PIN: the in-scope write must have LANDED and the
	// out-of-scope write must NOT exist. Under the pre-fix code the
	// rebuilt turn's fs_write was built with the ZERO scope which denies
	// every item, so the inside write would never land — this assertion
	// fails on pre-fix code.
	inside := filepath.Join(workingDir, "scope-inside", "inside.txt")
	content, err := os.ReadFile(inside)
	require.NoError(t, err, "the rebuilt scoped turn's fs_write must have landed the IN-scope file")
	require.Equal(t, "INSIDE_OK", string(content))
	outside := filepath.Join(workingDir, "outside.txt")
	_, err = os.Stat(outside)
	require.True(t, errors.Is(err, os.ErrNotExist),
		"the rebuilt scoped turn must NOT write outside its folder scope: %s exists", outside)

	// (d) DENIAL REASON PIN: the out-of-scope item's denial (the
	// ScopeDeniedError reason RunFSBatch copies into the denied item's
	// Error) must be fed back to the provider as the tool result.
	require.Contains(t, r2, "outside every folder scope",
		"the out-of-scope fs_write item's denial reason must be fed back to the provider as the tool result")
}
