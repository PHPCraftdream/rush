// P4 test-only ON-aware helper layer for the two-App Phase-4 scenarios.
// Every identifier carries the pp4 package tag. The helpers read the
// SELECTED authority's facts: flag ON (RUSH_BATCH_RUNTIME) they read
// batch_nodes / the App's real HostStore; flag OFF they keep the legacy
// async_jobs reads byte-for-byte. No helper mutates the flag: CI selects
// the mode. Handlers and goroutines must never call require through these
// helpers: the read helpers come in a plain (value, error) form plus a
// test-goroutine wrapper.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// pp4BatchRunner is the runner identity stamped on every pp4 batch fixture
// reservation (scenario D's unannounced root); the host is always the App's
// own registered HostStore host id so the production live reader's host
// probe answers alive.
const pp4BatchRunner = "pp4-fixture-runner"

// pp4StartedSeen reports whether lastTool IS the production started tool
// result. ON: the batch started response "batch root <uuid> started;
// completions arrive as selected notices." (batch_delivery.go), matched by
// the shared batchStartedRootID parser -- never by call id, which the
// started text does not carry. OFF: the legacy per-call literal, unchanged.
func pp4StartedSeen(lastTool, legacyCallID string) bool {
	if f6BatchOn() {
		return batchStartedRootID(strings.TrimSpace(lastTool)) != ""
	}
	return strings.Contains(lastTool, "Async bash job "+legacyCallID+" started")
}

// pp4CompletionSeen reports whether body carries the batch completion notice
// text for callID: FormatAsyncCompletion with an EMPTY tool name (batch
// leaves resolve by payload, not by tool identity), finished/failed or the
// host-lost interruption wording. OFF returns false: legacy handlers match
// their own literals.
func pp4CompletionSeen(body, callID string) bool {
	if !f6BatchOn() {
		return false
	}
	return strings.Contains(body, "Async job "+callID+" () finished") ||
		strings.Contains(body, "Async job "+callID+" () failed") ||
		strings.Contains(body, "Async job "+callID+" () was interrupted")
}

// pp4NextCallID returns the tool call id a provider script's default
// (re-issue) branch must use. OFF: always the legacy base id, byte-for-byte.
// ON: the base for the first emission, then a UNIQUE id per request -- the
// invocation hash includes the enclosing assistant message identity
// (batch_install_ingress.go Policy.MessageID), so re-attaching the same
// call id with a fresh message is correctly refused by the live-input fence.
func pp4NextCallID(base string, requestIndex int32) string {
	if !f6BatchOn() || requestIndex <= 1 {
		return base
	}
	return fmt.Sprintf("%s-r%d", base, requestIndex)
}

// pp4DriverHostID is the host identity the selected authority stamps on its
// claims: the App's real HostStore host id ON, the legacy AsyncJobStore's
// OFF. An empty id ON means the host never registered -- a bug this helper
// surfaces through the callers' comparisons.
func pp4DriverHostID(t *testing.T, application *App) string {
	t.Helper()
	if f6BatchOn() {
		return f6HostID(t, application)
	}
	return application.asyncJobStore.HostID()
}

// pp4CrashSimulate models a killed process ON: active work is killed and joined
// in a subprocess; idle fixtures close their empty Runner before releasing only
// the host lock. OFF retains the legacy crash seam unchanged.
func pp4CrashSimulate(t *testing.T, application *App, process ...*pcrCrashProcess) {
	t.Helper()
	if f6BatchOn() {
		if len(process) > 0 && process[0] != nil {
			require.NoError(t, process[0].pcrKillAndWait())
			return
		}
		if application.batchInstall != nil && application.batchInstall.Runner != nil {
			ctx, cancel := context.WithTimeout(context.Background(), appBatchDrainTimeout)
			defer cancel()
			runner := application.batchInstall.Runner
			observeCtx, cancelObserve := context.WithCancel(ctx)
			cancelObserve()
			events, err := runner.Events(observeCtx, 1)
			require.ErrorIs(t, err, context.Canceled)
			require.Zero(t, events.Head, "lock-only crash fixture Runner must have no observed work")
			require.NoError(t, runner.Close(ctx), "fixture Runner must be idle before lock-only crash")
		}
		require.NoError(t, application.BatchHostStore().SimulateCrashForTest())
		return
	}
	require.NoError(t, application.asyncJobStore.SimulateCrashForTest())
}

// pp4GracefulClose releases the App's host identity through the selected
// authority's REAL clean-exit path (a `rush run` process that finished
// normally): HostStore.Close ON, AsyncJobStore.Close OFF.
func pp4GracefulClose(t *testing.T, application *App) {
	t.Helper()
	ctx := context.Background()
	if f6BatchOn() {
		require.NoError(t, application.BatchHostStore().Close(ctx))
		return
	}
	require.NoError(t, application.asyncJobStore.Close(ctx))
}

// pp4JobFact is the authority-neutral readback of one job (root): Found
// false means the call never became work (or, for the deletion oracle, that
// it is gone). State/Cause/OutcomeCode are the root row's durable facts;
// Announced/Wake/Reacted mirror the legacy row columns (the notice and its
// accounting live on the root row: batchCommitNotice selects the root for
// one-task completions and recovery).
type pp4JobFact struct {
	Found                             bool
	State, Cause, OutcomeCode, HostID string
	Announced, Wake, Reacted          int64
}

// pp4JobFactRead is the non-failing read: it never calls require, so it is
// safe inside require.Eventually conditions and on helper goroutines. An
// unreadable authority reports ok=false, never a fake fact.
func pp4JobFactRead(application *App, owner, callID string) (fact pp4JobFact, ok bool) {
	if f6BatchOn() {
		err := application.DB().QueryRowContext(context.Background(),
			`SELECT state, cause, outcome_code, host_id, announced, wake, reacted
			 FROM batch_nodes
			 WHERE owner_session_id = ? AND tool_call_id = ? AND kind = 'batch' AND node_id = root_id
			 ORDER BY rowid DESC LIMIT 1`,
			owner, callID).Scan(&fact.State, &fact.Cause, &fact.OutcomeCode, &fact.HostID,
			&fact.Announced, &fact.Wake, &fact.Reacted)
		if errors.Is(err, sql.ErrNoRows) {
			return pp4JobFact{}, true
		}
		if err != nil {
			return pp4JobFact{}, false
		}
		fact.Found = true
		return fact, true
	}
	row, err := application.asyncJobStore.Get(context.Background(), owner, callID)
	if errors.Is(err, sql.ErrNoRows) {
		return pp4JobFact{}, true
	}
	if err != nil {
		return pp4JobFact{}, false
	}
	return pp4JobFact{
		Found: true, State: row.State, Cause: "", OutcomeCode: "", HostID: row.HostID,
		Announced: row.Announced, Wake: row.Wake, Reacted: row.Reacted,
	}, true
}

// pp4JobFactByCall is the test-goroutine wrapper: it fails the test on a
// read error and on a missing row.
func pp4JobFactByCall(t *testing.T, application *App, owner, callID string) pp4JobFact {
	t.Helper()
	fact, ok := pp4JobFactRead(application, owner, callID)
	require.True(t, ok, "job read failed for %s", callID)
	require.True(t, fact.Found, "no job row for call %s", callID)
	return fact
}

// pp4BatchRowsForRoot counts every batch_nodes row of one root (the subtree
// deletion oracle: a deleted unannounced root leaves zero rows). Legacy mode
// has no batch rows; callers gate on f6BatchOn.
func pp4BatchRowsForRoot(t *testing.T, application *App, rootID string) int64 {
	t.Helper()
	var rows int64
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM batch_nodes WHERE root_id = ?`, rootID).Scan(&rows))
	return rows
}

// pp4ReserveUnannouncedRoot reserves a constraint-valid batch root with ONE
// running command task child and deliberately skips BatchAnnounceStarted:
// announced=0, the shape a host killed between Claim and the ack gate leaves
// (scenario D). The host MUST be the App's own registered HostStore id so
// the recovery election sees a dead OWN host after pp4CrashSimulate.
func pp4ReserveUnannouncedRoot(t *testing.T, application *App, owner, toolCallID string) (root, node string) {
	t.Helper()
	res, err := session.NewBatchStore(application.DB()).Reserve(context.Background(), session.BatchReservation{
		Owner: owner, Host: f6HostID(t, application), RunnerID: pp4BatchRunner, ToolCallID: toolCallID,
		InputHash: "pp4-hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none",
		}, Children: []session.BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true},
			State: "running", ControlState: "none",
		}}}},
	})
	require.NoError(t, err)
	root, node = res.Snapshot.Root.RootID, res.Snapshot.Nodes[1].NodeID
	// Pin the fixture's own premise: unannounced, live rows only.
	var announced int64
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT announced FROM batch_nodes WHERE node_id = ?`, root).Scan(&announced))
	require.EqualValues(t, 0, announced, "the fixture root must be unannounced")
	return root, node
}

// pp4SelectedNoticeCount counts the session's persisted background-job
// messages that carry a SELECTED notice kind (task / batch / root). Under the
// batch runtime every detached root also persists its STARTED announcement
// (BatchAnnounceStarted: a background-job message with an EMPTY notice kind);
// that announcement is not a delivered or recovered notice and must not be
// counted as one, so the batch scenarios count by kind instead of taking the
// total.
func pp4SelectedNoticeCount(t *testing.T, application *App, sessionID string) int {
	t.Helper()
	msgs, err := application.Messages.List(context.Background(), sessionID)
	require.NoError(t, err)
	n := 0
	for _, m := range msgs {
		if m.BackgroundJobNotice && m.NoticeKind != "" {
			n++
		}
	}
	return n
}

// pp4PullNotices moves the owner's pending selected notices into history
// through the selected authority's pull: BatchPullNotices with the
// production-shaped notice builder ON, the legacy PullJobNotices with the
// file's own legacy builder OFF. Returns how many notices were delivered.
func pp4PullNotices(t *testing.T, application *App, owner string) int {
	t.Helper()
	ctx := context.Background()
	if f6BatchOn() {
		pulled, err := session.NewBatchStore(application.DB()).BatchPullNotices(
			ctx, application.Messages, owner, pp4BuildBatchNoticeParams)
		require.NoError(t, err)
		return len(pulled)
	}
	pulled, err := application.asyncJobStore.PullJobNotices(
		ctx, application.Messages, owner, recoveryScenarioEBuildJobNoticeParams)
	require.NoError(t, err)
	return len(pulled)
}

// pp4BuildBatchNoticeParams mirrors the production buildBatchNoticeMessage
// Params (agent_notice_pull.go) for the test's direct pull: batch notices
// carry NO tool name; host-lost/interrupted rows are interruptions; the
// message's NoticeKind is the row's own selected notice kind.
func pp4BuildBatchNoticeParams(row session.BatchNoticeRow) message.CreateMessageParams {
	recovered := row.State == "interrupted" || row.OutcomeCode == "host_lost"
	completion := agent.AsyncCompletion{
		ToolCallID: row.ToolCallID,
		ToolName:   "",
		Content:    row.OutcomeSummary,
		IsError:    row.State == "failed" || row.OutcomeCode == "failed",
		TimedOut:   row.State == "timed_out",
		Stopped:    row.Cause == "stop" && !recovered,
		Cancelled:  row.Cause == "stop" && row.State == "cancelled",
	}
	if row.Cause == "host_closing" || recovered {
		completion.Interrupted = true
	}
	origin := message.OriginWeb
	if row.OriginCLI {
		origin = message.OriginCLI
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: agent.FormatAsyncCompletion(completion)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		NoticeKind:          row.Kind,
		Origin:              origin,
	}
}
