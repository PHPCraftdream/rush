package agent

// R12-H2 chunk D, acceptance item 8 (design §3 row "Held question").
//
// Production entry points exercised (the batch_disk_provider_question_test.go
// harness): the REAL agent-kind executor family (batchAgentFamily →
// batchCoordinatorAgentRuns driver → frozen child-driver call), the REAL
// held-question publication (batchCoordinatorQuestionPublisher →
// session_notices), the REAL jobbatch Runner.Inject answering the SAME
// node/token/depth, and the REAL RunnerHandle.Stop.
//
// TestHeldQuestionInject:
//   - fs write → ask → Runner.Inject (same node/token/depth) → fs read/write:
//     both filesystem operations hit the SAME provider instance with the
//     SAME binding id (one leaf run, op2's provider pointer identical to
//     op1's); the answer lands at the held depth; no replacement
//     batch/provider registration happens (exactly one root for the owner,
//     both OS targets untouched).
//   - The owner is released only after scope/driver closure: after settle +
//     dispose and the fixture's Construction release the binding is deleted.
//
// TestStopWhileHeldKeepsQuestionAndRefusesLateInject (the §3 row's separate
// Stop-while-held case):
//   - the Stop ACK returns while the question is held; the durable question
//     notice stays pending and the physical write count is unchanged — Stop
//     does NOT settle admission or tear down the durable scope synchronously;
//   - the frozen incarnation is NOT answerable after Stop: the scope's stop
//     latches (batchAgentScope.stopped) make deliver refuse, so the SAME
//     Inject is refused and no second provider write or replacement batch
//     ever happens; the node then settles through the real unwind;
//   - ownership survives the Stop acknowledgement until settle + dispose:
//     only after the fixture's Construction release is the binding deleted.
//
// Production mutation caught: answering a held question by submitting a
// REPLACEMENT batch (or rebuilding the child context without the provider
// stamp) would run the second filesystem op on a different/OS provider —
// visible as a second root, a second leaf incarnation, or sentinel mutation
// on the exact OS targets. A Stop that settled the node or released the
// binding owners on cancellation observation alone would delete the binding
// while the physical unwind is still in flight; an Inject accepted into a
// stopped incarnation would revive cancelled work.
//
// Housekeeping: both tests release their own registration/Construction
// owners before expecting deletion and assert the borrowed provider is
// never closed by Rush (ownerCount reaches zero; the fixture provider type
// has no Close hook to call).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestHeldQuestionInject(t *testing.T) {
	env := newQuestionInstall(t)
	root := env.submitAgentRoot()

	// The leaf wrote op1, then parked in the production held question.
	var text string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&text) == nil
	}, 5*time.Second, 5*time.Millisecond, "the held question is published")
	node, tok, ok := parseBatchQuestionMarker(text)
	require.True(t, ok, "the notice carries the production marker: %q", text)
	require.NotEmpty(t, node)
	require.Greater(t, tok, int64(0), "the incarnation token is nonzero")
	require.Contains(t, text, "depth=1", "the answer must land at the held depth")

	// While held: the binding stays resolvable and the node is live.
	require.Equal(t, 1, env.fake.writeCount(), "no second op before the answer")
	_, err := env.install.DiskBindings.Lookup(env.bindingID)
	require.NoError(t, err, "the binding stays resolvable while the question is held")
	var state string
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		string(root), env.sessionID).Scan(&state))
	require.False(t, jobbatch.State(state).Terminal(),
		"the held question keeps the incarnation open (state %q)", state)

	// The answer enters through the REAL Runner.Inject seam, addressing the
	// SAME node/token/depth the marker carries.
	runner := env.install.RunnerHandle.runner
	require.NoError(t, runner.Inject(context.Background(), jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"text":"yes, proceed"}`)),
		"the answer is accepted into the SAME held incarnation")

	// The SAME incarnation wakes and finishes op2; the node settles.
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			string(root), env.sessionID).Scan(&state) == nil && state == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the answered incarnation settles completed")

	op1, op2, b1, b2, midOK, answer := env.leaf.snapshot()
	require.True(t, midOK, "the binding was resolvable between op1 and op2")
	require.True(t, op2 == op1, "op2 ran on the SAME provider instance as op1")
	require.Equal(t, env.bindingID, b1)
	require.Equal(t, b1, b2, "the SAME binding id is carried into the answered incarnation")
	require.Equal(t, "yes, proceed", string(answer), "the injected answer reached the asker at depth 1")
	require.Equal(t, 2, env.fake.writeCount(), "both filesystem operations hit the provider")
	require.Equal(t, 1, env.leaf.runCount(), "one leaf incarnation: no replacement driver call")

	// No replacement batch/provider registration: exactly ONE root for the
	// owner, and the OS targets op1/op2 wrote to are untouched.
	var roots int
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?1 AND kind = 'batch' AND parent_id IS NULL`,
		env.sessionID).Scan(&roots))
	require.Equal(t, 1, roots, "no replacement batch was submitted")
	for p, want := range map[string]string{env.path1: "q8-sentinel-1", env.path2: "q8-sentinel-2"} {
		sentinel, err := os.ReadFile(filepath.FromSlash(p))
		require.NoError(t, err)
		require.Equal(t, want, string(sentinel), "the exact OS target %s was never touched", p)
	}

	// REAL release trigger for the lingering Execution owner (R12-H2 §1.2):
	// the answered root settled completed, but jobbatch disposes a settled
	// root's execution ONLY on root release — Runner.Release → EventRelease
	// → handle.giveUp → maybeDispose → the physical-Dispose wrapper drops
	// the Execution owner (internal/jobbatch/runner_control.go). The root is
	// the one submitAgentRoot returned.
	require.NoError(t, env.install.Runner.Release(context.Background(), root))

	// Owner release only after scope/driver closure: settle + dispose
	// dropped the root/execution owners; release the fixture's Construction
	// owner and the binding is deleted (the owner set empties).
	env.releaseConstruction()
	require.Eventually(t, func() bool { return env.install.DiskBindings.ownerCount() == 0 },
		5*time.Second, time.Millisecond,
		"no provider owner remains once every legitimate owner ended")
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding unavailable after scope/driver closure")
}

func TestStopWhileHeldKeepsQuestionAndRefusesLateInject(t *testing.T) {
	env := newQuestionInstall(t)
	root := env.submitAgentRoot()

	var text string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&text) == nil
	}, 5*time.Second, 5*time.Millisecond, "the held question is published")
	node, tok, ok := parseBatchQuestionMarker(text)
	require.True(t, ok, "the notice carries the production marker: %q", text)
	require.Greater(t, tok, int64(0), "the incarnation token is nonzero")

	// Drive the REAL Stop while the question is held: the acknowledgement
	// returns without settling the node or answering the question.
	require.NoError(t, env.install.RunnerHandle.Stop(context.Background(), root))
	require.Equal(t, 1, env.fake.writeCount(), "Stop never runs the second filesystem op")
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
		env.sessionID, session.NoticeKindChildQuestion).Scan(&text),
		"the question notice stays durably pending across the Stop ack")
	_, err := env.install.DiskBindings.Lookup(env.bindingID)
	require.NoError(t, err, "the binding owner survives the Stop acknowledgement (release is dispose-only)")

	// The frozen incarnation is NOT answerable after Stop: the scope's stop
	// latch (batchAgentScope.stopped) makes deliver refuse, and the terminal
	// node refuses through the runner. Either way the SAME Inject is refused,
	// never buffered for a revived incarnation.
	runner := env.install.RunnerHandle.runner
	require.Eventually(t, func() bool {
		return runner.Inject(context.Background(), jobbatch.ID(node),
			json.RawMessage(`{"depth":1,"text":"late"}`)) != nil
	}, 5*time.Second, 5*time.Millisecond, "a post-Stop Inject into the held incarnation is refused")

	// The incarnation unwinds through the real engine; op2 never runs, no
	// replacement batch appears, and the OS targets stay untouched.
	var state string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(context.Background(),
			`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			string(root), env.sessionID).Scan(&state) == nil && jobbatch.State(state).Terminal()
	}, 5*time.Second, 5*time.Millisecond, "the stopped incarnation settles terminal")
	require.Equal(t, 1, env.fake.writeCount(), "op2 never ran after the Stop")
	var roots int
	require.NoError(t, env.conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?1 AND kind = 'batch' AND parent_id IS NULL`,
		env.sessionID).Scan(&roots))
	require.Equal(t, 1, roots, "Stop never submits a replacement batch")
	for p, want := range map[string]string{env.path1: "q8-sentinel-1", env.path2: "q8-sentinel-2"} {
		sentinel, err := os.ReadFile(filepath.FromSlash(p))
		require.NoError(t, err)
		require.Equal(t, want, string(sentinel), "the exact OS target %s was never touched", p)
	}

	// REAL release trigger for the lingering Execution owner (R12-H2 §1.2):
	// the stopped root settled terminal, but jobbatch disposes a settled
	// root's execution ONLY on root release — Runner.Release → EventRelease
	// → handle.giveUp → maybeDispose → the physical-Dispose wrapper drops
	// the Execution owner (internal/jobbatch/runner_control.go). The root is
	// the one submitAgentRoot returned.
	require.NoError(t, env.install.Runner.Release(context.Background(), root))

	// Owner release only after scope/driver closure: settle + dispose
	// dropped the root/execution owners; the fixture's Construction release
	// empties the owner set and deletes the binding.
	env.releaseConstruction()
	require.Eventually(t, func() bool { return env.install.DiskBindings.ownerCount() == 0 },
		5*time.Second, time.Millisecond,
		"no provider owner remains once every legitimate owner ended")
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding unavailable after scope/driver closure")
}
