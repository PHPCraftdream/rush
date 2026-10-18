package agent

// R12-H2 chunk D, acceptance item 5 (design §3 row "Real Run/Add").
//
// Production entry points exercised: the installed model-facing batch_run
// and batch_add tools (batch_model_tools.go newBatchModelTools) backed by
// the REAL installedBatchModelControl.Run/Add (batch_model_control.go):
// admission (ValidateSubmit/ValidateAdd), the Root owner
// acquire-before-Submit/adopt-on-Done via the shared batchRootLeaseStore,
// Add's rootLeases.matches binding check, and nested/parallel scheduling in
// the real engine. Provider gates keep the root and its nested parent open
// while Add runs.
//
// Oracles: the initial Run leaf and every added leaf (sequential AND
// parallel nested items) execute through the SAME binding with DISTINCT
// keys; a second registered provider (binding B) cannot Add into A's tree;
// each executed leaf runs exactly once (hooks/restrictions run once per
// executed leaf, inside the pinned policy leaf); a denial (unknown tool)
// has no provider side effect. The Run template is DETACHED so batch_run
// returns the root id while the provider gate holds the leaf open — the
// open root is what every batch_add targets.
//
// Production mutation caught: an Add that re-resolves the provider instead
// of inheriting the template's pinned binding would let binding B join A's
// tree (cross-provider routing); a Run that released the Root owner before
// Submit (or skipped the adopt) would let the construction/binding die
// beneath a live tree.
//
// Housekeeping: the test releases the gate, waits for the real settlement,
// then releases its Construction owner and asserts the owner set empties.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// h2aTestProfile mirrors the installed capacity profile the fixtures use.
func h2aTestProfile(t *testing.T) *BatchProfile {
	t.Helper()
	profile, err := NewBatchProfile(BatchProfileCaps{IO: 4, CLIDriver: 4, AgentDriver: 4, Default: 4, Global: 16})
	require.NoError(t, err)
	return profile
}

// inventorySnapshot reads both counters for exact before/after comparison.
func inventorySnapshot(c *inventoryCounters) [2]int64 {
	return [2]int64{c.hooks.Load(), c.perms.Load()}
}

func TestModelControlRunAddInheritance(t *testing.T) {
	// Review P1-7b: the PINNED leaves carry counting hook + permission
	// layers (the inventoryWrappedLeaf layering: counting hook outermost,
	// production restrictedTool with a counting authorizer inside, the
	// capture leaf innermost) so "hooks/restrictions run once per executed
	// leaf" is observable on the real pinned path — not inferred from the
	// leaf run count.
	counters := &inventoryCounters{}
	env := newH2aGatedInstallWithLeafWrap(t, func(inner fantasy.AgentTool) fantasy.AgentTool {
		restricted := restrictedTool{AgentTool: inner, authorizer: inventoryPermAuthorizer{c: counters}}
		return inventoryHookLeaf{AgentTool: restricted, c: counters}
	})
	env.fake.arm()

	dir := env.targetPath[:len(env.targetPath)-len("out.txt")]
	path1 := dir + "run-1.txt"
	path2 := dir + "add-seq-a.txt"
	path3 := dir + "add-seq-b.txt"
	path4 := dir + "add-par.txt"

	// The installed control over the REAL runner/delivery/root-lease seams.
	// batchTestSessions makes readAuthorized resolve the calling session; the
	// host/runner id come from the install so Add's live-local fence matches.
	control := NewInstalledBatchModelControl(
		env.install.RunnerHandle, env.install.Delivery, env.store, nil,
		batchTestSessions(env.sessionID),
		env.install.Runtime.cfg.Host, env.install.Runtime.cfg.RunnerID,
		h2aTestProfile(t), nil, env.install.RootLeases,
	)
	reg, err := env.registries.Resolve(env.registryID)
	require.NoError(t, err)
	template := BatchInvocation{
		SessionID: env.sessionID, MessageID: "m-1", RegistryID: env.registryID,
		Origin: "coordinator",
		// Detached: batch_run returns the root id while the gated leaf keeps
		// the root open — exactly the live root every batch_add below targets.
		Delivery: batchDeliveryChoice{Mode: batchDeliveryDetached},
		Policy: batchLeafPolicyContext{
			RegistryID: env.registryID, RestrictedToolset: env.fingerprint,
			DiskProvider: true, DiskBindingID: env.bindingID,
		},
	}
	tools, err := control.InstallBatchModelControls(reg, template)
	require.NoError(t, err)
	var runTool, addTool fantasy.AgentTool
	for _, tool := range tools {
		switch tool.Info().Name {
		case "batch_run":
			runTool = tool
		case "batch_add":
			addTool = tool
		}
	}
	require.NotNil(t, runTool)
	require.NotNil(t, addTool)

	// batch_run (detached): the root id returns while the leaf parks in the
	// provider gate — the root stays open behind the gate.
	ctx := batchCallCtx(env.sessionID)
	runInput, err := json.Marshal(batchModelParams{Spec: batchModelSpec{
		Mode: "parallel", Notify: "each", MaxParallel: 2,
		Items: []batchModelItem{{Tool: "fs_write", Input: json.RawMessage(h2aLeafInput(path1))}},
	}})
	require.NoError(t, err)
	runRoot := make(chan string, 1)
	runErr := make(chan error, 1)
	go func() {
		resp, err := runTool.Run(ctx, fantasy.ToolCall{ID: "call-run-1", Name: "batch_run", Input: string(runInput)})
		if err != nil {
			runErr <- err
			return
		}
		var root string
		if uerr := json.Unmarshal([]byte(resp.Content), &root); uerr != nil {
			runErr <- uerr
			return
		}
		runRoot <- root
	}()
	var rootID string
	select {
	case rootID = <-runRoot:
	case err := <-runErr:
		require.FailNow(t, "detached batch_run failed: %v", err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "detached batch_run never returned the root id")
	}
	require.NotEmpty(t, rootID, "batch_run returns the root id")

	// While the root is held open, batch_add appends a sequential nested
	// batch (two distinct keys) and a parallel leaf (a third key) — all
	// targeted at the live root.
	addDeadline := time.Now().Add(5 * time.Second)
waitInFlight:
	for {
		select {
		case <-env.leaf.inFlight:
			break waitInFlight
		default:
		}
		if time.Now().After(addDeadline) {
			require.FailNow(t, "the Run leaf never reached its provider gate")
		}
		time.Sleep(time.Millisecond)
	}
	addInput, err := json.Marshal(batchModelParams{Root: rootID, Items: []batchModelItem{
		{Batch: &batchModelSpec{Mode: "sequential", Items: []batchModelItem{
			{Tool: "fs_write", Input: json.RawMessage(h2aLeafInput(path2))},
			{Tool: "fs_write", Input: json.RawMessage(h2aLeafInput(path3))},
		}}},
		{Tool: "fs_write", Input: json.RawMessage(h2aLeafInput(path4))},
	}})
	require.NoError(t, err)
	addResp, err := addTool.Run(ctx, fantasy.ToolCall{ID: "call-add-1", Name: "batch_add", Input: string(addInput)})
	require.NoError(t, err)
	require.False(t, addResp.IsError, "add response: %s", addResp.Content)
	var added []jobbatch.ID
	require.NoError(t, json.Unmarshal([]byte(addResp.Content), &added))
	require.Len(t, added, 2, "the nested batch and the parallel leaf are added")

	// Provider B cannot Add into A's tree: the same live root, a template
	// pinned to a DIFFERENT registered binding is refused by the root
	// lease's binding identity match — with no side effect on either
	// provider.
	other := newH2aGatingDisk()
	otherBinding, err := env.install.DiskBindings.RegisterProvider(other)
	require.NoError(t, err)
	foreignTemplate := template
	foreignTemplate.Policy.DiskBindingID = otherBinding
	foreignTools, err := control.InstallBatchModelControls(reg, foreignTemplate)
	require.NoError(t, err)
	var foreignAdd fantasy.AgentTool
	for _, tool := range foreignTools {
		if tool.Info().Name == "batch_add" {
			foreignAdd = tool
		}
	}
	require.NotNil(t, foreignAdd)
	countersBeforeRefusals := inventorySnapshot(counters)
	foreignResp, err := foreignAdd.Run(ctx, fantasy.ToolCall{ID: "call-add-2", Name: "batch_add", Input: string(addInput)})
	require.NoError(t, err, "control tools report denials in the envelope, not as errors")
	require.True(t, foreignResp.IsError, "provider B must not be able to Add into A's tree")
	require.Zero(t, other.writeCount(), "the refused binding has no provider side effect")

	// Denial: an unknown tool in Add is refused before any submission or
	// provider side effect.
	badInput, err := json.Marshal(batchModelParams{Root: rootID, Items: []batchModelItem{
		{Tool: "definitely-not-pinned", Input: json.RawMessage(`{}`)},
	}})
	require.NoError(t, err)
	badResp, err := addTool.Run(ctx, fantasy.ToolCall{ID: "call-add-3", Name: "batch_add", Input: string(badInput)})
	require.NoError(t, err)
	require.True(t, badResp.IsError, "the unknown tool is denied")
	require.Zero(t, env.fake.writeCount(), "denials have no provider side effect")
	// The refused foreign Add and the unknown-tool denial added ZERO hook
	// firings and ZERO permission asks: neither ever reached a pinned leaf.
	require.Equal(t, countersBeforeRefusals, inventorySnapshot(counters),
		"refusals and denials must not fire hooks or permission asks")

	// Release the gate: every executed leaf writes its DISTINCT key through
	// the SAME provider; hooks/restrictions ran exactly once per executed
	// leaf (four executions, no wrapper double-run).
	env.leaf.letGo()
	env.fake.letGo()
	require.Eventually(t, func() bool { return env.leaf.runCount() == 4 },
		5*time.Second, time.Millisecond, "each executed leaf ran exactly once (got %d)", env.leaf.runCount())
	// Review P1-7b oracle: hooks == 4 and permission asks == 4 — exactly
	// once per EXECUTED leaf (four executions), not per submission and not
	// zero.
	require.Equal(t, int64(4), counters.hooks.Load(),
		"the counting hook layer fired exactly once per executed leaf")
	require.Equal(t, int64(4), counters.perms.Load(),
		"the permission gate was asked exactly once per executed leaf")
	for _, p := range []string{path1, path2, path3, path4} {
		got, ok := env.fake.has(p)
		require.True(t, ok, "distinct key %s must be written through provider A", p)
		require.Equal(t, "lifecycle-data", got)
	}
	require.Zero(t, other.writeCount())

	// REAL release trigger for the lingering Execution owner (R12-H2 §1.2):
	// the batch_run root and its root-lease-adopted tree settled, but
	// jobbatch disposes a settled root's execution ONLY on root release —
	// Runner.Release → EventRelease → handle.giveUp → maybeDispose → the
	// physical-Dispose wrapper drops the Execution owner
	// (internal/jobbatch/runner_control.go). Releasing the root releases the
	// whole adopted tree with it.
	require.NoError(t, env.install.Runner.Release(context.Background(), jobbatch.ID(rootID)))

	// Housekeeping: after settlement every engine owner is gone; release the
	// fixture's Construction owner and assert the owner set empties.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding unavailable after all owners end")
	require.Equal(t, 0, env.install.DiskBindings.ownerCount())
}
