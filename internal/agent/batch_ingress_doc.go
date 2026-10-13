package agent

// R11C ingress core — mapping, contracts and boundaries.
//
// Old -> new:
//
//	session-keyed leaf lookup (batch_exec.go env.Leaves(sessionID))
//	  -> pinned-registry lookup: batchLeafPayload.RegistryID +
//	     batchLeafLookupID; batchRuntime.LeafEnv resolves by registry
//	     identity, so concurrent calls with different restricted toolsets,
//	     folder scopes or disks never collide (research doc section 3).
//	three-value context copy in batch_exec_generic.go (message id, image
//	  support, model name)
//	  -> batchLeafPolicyContext in the payload + batchLeafRunContext, which
//	     carries the folder scope, restricted-run allowlist and custom disk
//	     across as well. R11F replaces the family-side copy with it.
//	many ingress callers, legacy async wrappers
//	  -> batchRuntime.Invoke: one validate -> D1 refusal -> admission ->
//	     live-attach -> Submit -> delivery path; hooks/policy run once in
//	     the pinned leaf.
//	implicit execution identity
//	  -> BatchInvocation: session/message/call ids, pinned registry
//	     identity, origin, delivery mode/window, background-escape policy,
//	     waiter node/rank, validated fail-closed and canonicalised once.
//
// Where the other packages plug in:
//
//	R11D implements batchDelivery (DeliverBatch) for sync/inline/detached
//	elections and the background escape, and owns cancellation (sync
//	caller cancel = Runner.Stop through batchRunnerHandle).
//	R11E owns the app lifecycle around the Runner (fault exit, drain).
//	R11F installs: binds batchRunnerHandle over *jobbatch.Runner,
//	implements batchAttachStore over session.BatchStore.ReserveIdentity,
//	wires LeafEnv into the executor construction, points cli_tool_bridge
//	pinnedInvoke and agentic_fetch at Invoke, and arms the families with
//	batchLeafRunContext.
//
// Assumed engine contracts:
//
//	*jobbatch.Runner roots are caller-supplied; Submit echoes the
//	requested root ID (internal/jobbatch/runtime_types.go).
//	R11B ReserveIdentity keys live roots on (owner session, tool call id)
//	with input-hash, owning-host and runner-incarnation fences; the
//	commit hook's submit hash (batchCommitSpecHash) agrees with
//	batchInvocationHash because both hash the accepted one-item spec.
//	R1 policyLeafRegistry.resolve is read-only and fails closed on an
//	unknown tool; a registry is identified by its construction, not its
//	session.
//	fantasy.ToolCall.ID is the stable tool-call identity of a retry.
//
// NOT done here (by design):
//
//	delivery elections, inline/detached handling, background escape,
//	cancellation semantics (R11D); app lifecycle and fault exit (R11E);
//	installation into coordinator_tools.go, cli_tool_bridge.go,
//	agentic_fetch_policy.go or app.go (R11F); explicit specs and Notify
//	selection (R13); MCP kind declaration wiring (R7a).
type batchIngressDoc struct{}
