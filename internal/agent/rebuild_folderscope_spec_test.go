package agent

// T12 round-trip unit tests: the durable row's persisted folder-scope
// spec must survive the JSON boundary, and RebuildSessionAgentCall must
// recompile it into the rebuilt call's CallOptions — with a corrupted
// spec degrading fail-closed (deny-everything), never unscoped.

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

// TestRebuildSessionAgentCall_RestoresFolderScopeFromSpec pins the
// durable-restart half of T12: a SessionAgentCallData row carrying a
// FolderScopeSpec must be rebuilt into a call whose compiled FolderScope
// is bound in CallOptions (which never survives the queue handoff) and
// whose spec round-trips unchanged.
func TestRebuildSessionAgentCall_RestoresFolderScopeFromSpec(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). RebuildSessionAgentCall canonicalizes the durable spec
	// through the REAL disk (tools.CanonicalizeFolderScopeSpec) while the
	// assertions below check paths built from workDir itself -- a raw
	// t.TempDir() would put the compiled scope root and the checked paths
	// in different namespaces. Same class already fixed in 73878311 /
	// b253bb70.
	workDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	data := session.SessionAgentCallData{
		SessionID: "scope-rebuild-probe",
		Prompt:    "hello",
		FolderScopeSpec: &session.FolderScopeSpec{
			WorkingDir: workDir,
			Entries: []session.FolderScopeEntry{
				{
					Dir: "scope-inside",
					Ops: []session.FileOp{session.FileOp("create"), session.FileOp("overwrite")},
				},
			},
			KeepCommandTools: false,
		},
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err)

	// (1) The rebuilt call carries a compiled scope in CallOptions.
	require.NotNil(t, call.CallOptions,
		"a rebuilt call whose row carries a folder-scope spec must carry CallOptions")
	require.NotNil(t, call.CallOptions.FolderScope,
		"a rebuilt call whose row carries a folder-scope spec must carry a compiled FolderScope")

	// (2) The relative entry resolves against the spec's WorkingDir:
	// granted ops inside the scope pass, non-granted ops and paths
	// outside the scope are typed denials.
	inside := filepath.Join(workDir, "scope-inside", "f.txt")
	require.NoError(t, call.CallOptions.FolderScope.Check(inside, permission.FileOpCreate),
		"an op granted by the recompiled scope must be allowed inside the scoped subtree")
	var denied *permission.ScopeDeniedError
	require.ErrorAs(t, call.CallOptions.FolderScope.Check(inside, permission.FileOpDelete), &denied,
		"an op not granted by the scope must be a typed denial, not an allow")
	require.ErrorAs(t, call.CallOptions.FolderScope.Check(
		filepath.Join(workDir, "outside.txt"), permission.FileOpCreate), &denied,
		"a path outside every scoped subtree must be a typed denial")

	// (3) The spec round-trips exactly through the rebuild.
	require.NotNil(t, call.FolderScopeSpec)
	require.Equal(t, workDir, call.FolderScopeSpec.WorkingDir)
	require.Len(t, call.FolderScopeSpec.Entries, 1)
	require.Equal(t, "scope-inside", call.FolderScopeSpec.Entries[0].Dir)
	require.Equal(t, []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite}, call.FolderScopeSpec.Entries[0].Ops)
	require.False(t, call.FolderScopeSpec.KeepCommandTools)
}

// TestRebuildSessionAgentCall_CorruptedFolderScopeSpecFailsClosed pins
// the fail-closed degradation: a spec that cannot recompile must NOT
// fail the rebuild (the row still runs and can talk), but the compiled
// scope it binds is the zero FolderScope, which denies every operation —
// never an unscoped turn on the shared legacy file surface.
func TestRebuildSessionAgentCall_CorruptedFolderScopeSpecFailsClosed(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)

	workDir := t.TempDir()
	data := session.SessionAgentCallData{
		SessionID: "scope-corrupt-probe",
		Prompt:    "hello",
		FolderScopeSpec: &session.FolderScopeSpec{
			WorkingDir: workDir,
			Entries:    []session.FolderScopeEntry{{Dir: "", Ops: nil}},
		},
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), data)
	require.NoError(t, err, "a corrupted spec must not fail the rebuild; it degrades fail-closed instead")

	require.NotNil(t, call.CallOptions)
	require.NotNil(t, call.CallOptions.FolderScope)

	var denied *permission.ScopeDeniedError
	require.ErrorAs(t, call.CallOptions.FolderScope.Check(
		filepath.Join(workDir, "any.txt"), permission.FileOpCreate), &denied,
		"a corrupted spec must compile to a deny-everything scope (create denied)")
	require.ErrorAs(t, call.CallOptions.FolderScope.Check(
		filepath.Join(workDir, "any.txt"), permission.FileOpRead), &denied,
		"a corrupted spec must compile to a deny-everything scope (read denied)")
}

// TestSessionAgentCallDataFolderScopeSpecJSONRoundTrip pins the durable
// JSON boundary for T12: a non-nil folder-scope spec survives
// marshal/unmarshal and both conversion layers intact, and a nil spec
// serializes as `"FolderScopeSpec":null` (untagged pointer field) and
// decodes back to a nil pointer — keeping "no scope" distinguishable
// from "scope present but empty".
func TestSessionAgentCallDataFolderScopeSpecJSONRoundTrip(t *testing.T) {
	workDir := t.TempDir()
	call := SessionAgentCall{
		SessionID: "json-scope-roundtrip-probe",
		Prompt:    "hello",
		FolderScopeSpec: &permission.FolderScopeSpec{
			WorkingDir: workDir,
			Entries: []permission.FolderScopeEntry{
				{Dir: "sub", Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpRead}},
			},
			KeepCommandTools: true,
		},
	}

	data := ToSessionAgentCallData(call)
	raw, err := json.Marshal(data)
	require.NoError(t, err)

	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotNil(t, decoded.FolderScopeSpec, "a present spec must survive the JSON boundary")
	require.Equal(t, workDir, decoded.FolderScopeSpec.WorkingDir)
	require.Equal(t, []session.FolderScopeEntry{
		{Dir: "sub", Ops: []session.FileOp{"create", "read"}},
	}, decoded.FolderScopeSpec.Entries)
	require.True(t, decoded.FolderScopeSpec.KeepCommandTools)

	roundTripped, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	require.NotNil(t, roundTripped.FolderScopeSpec)
	require.Equal(t, workDir, roundTripped.FolderScopeSpec.WorkingDir)
	require.Equal(t, []permission.FolderScopeEntry{
		{Dir: "sub", Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpRead}},
	}, roundTripped.FolderScopeSpec.Entries)
	require.True(t, roundTripped.FolderScopeSpec.KeepCommandTools)

	// Nil spec: SessionAgentCallData has no json tags at all (every
	// field, including this one, marshals under its exact Go name with no
	// omitempty), so a nil spec serializes as `"FolderScopeSpec":null`,
	// not as an absent key. It must unmarshal back to a nil Go pointer
	// through both layers — that is what keeps "no scope declared"
	// distinguishable from "scope present but empty".
	nilCall := SessionAgentCall{SessionID: "json-scope-roundtrip-nil", Prompt: "hello"}
	nilData := ToSessionAgentCallData(nilCall)
	nilRaw, err := json.Marshal(nilData)
	require.NoError(t, err)
	require.Contains(t, string(nilRaw), `"FolderScopeSpec":null`,
		"a nil spec must serialize as a JSON null (consistent with this struct's other untagged pointer fields), not be silently dropped")

	var nilDecoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(nilRaw, &nilDecoded))
	require.Nil(t, nilDecoded.FolderScopeSpec,
		"a row without a folder-scope spec must decode to a nil spec, never an inert zero-value spec")
	nilRoundTripped, err := FromSessionAgentCallData(nilDecoded)
	require.NoError(t, err)
	require.Nil(t, nilRoundTripped.FolderScopeSpec)
}
