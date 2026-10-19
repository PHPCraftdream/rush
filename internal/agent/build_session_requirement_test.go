package agent

// R12-H2b §2.2 unit pin for the durable requirement envelope builder: the
// fence is the ONLY policy baseline a continuation restores from, so an
// envelope silently dropping any policy field (folder scope, allowlist
// spec, call policy) would make a rebuilt continuation execute under a
// WEAKER policy than the originating invocation admitted. Each assertion
// below targets one such dropped-field mutation.

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBuildSessionExecutionRequirement_Fields(t *testing.T) {
	require.Nil(t, BuildSessionExecutionRequirement("", "app", 1, nil, permission.RunAllowlistSpec{}, &CallOptions{}),
		"an empty binding id must never produce a fake envelope")
	require.Nil(t, BuildSessionExecutionRequirement("b1", "app", 1, nil, permission.RunAllowlistSpec{}, nil),
		"nil call options must never produce a fake envelope")

	folderScope := &permission.FolderScopeSpec{
		WorkingDir: "workdir",
		Entries: []permission.FolderScopeEntry{{
			Dir: "scope-inside",
			Ops: []permission.FileOp{permission.FileOpRead},
		}},
	}
	allowlist := permission.RunAllowlistSpec{Restrict: true, AllowTools: []string{"read"}}
	opts := &CallOptions{TimeoutHardCap: 7 * time.Minute, TimeoutOptionsSet: true}

	got := BuildSessionExecutionRequirement("b1", "app", 7, folderScope, allowlist, opts)
	require.NotNil(t, got)
	require.Equal(t, "b1", got.BindingID)
	require.Equal(t, "app", got.Owner)
	require.Equal(t, int64(7), got.Epoch)

	require.NotNil(t, got.FolderScopeSpec, "folder-scope baseline must not be dropped")
	require.Equal(t, "workdir", got.FolderScopeSpec.WorkingDir)
	require.Len(t, got.FolderScopeSpec.Entries, 1)
	require.Equal(t, "scope-inside", got.FolderScopeSpec.Entries[0].Dir)
	require.Equal(t, session.FileOp(permission.FileOpRead), got.FolderScopeSpec.Entries[0].Ops[0])

	require.NotNil(t, got.RunAllowlistSpec, "restricted-run baseline must not be dropped")
	require.True(t, got.RunAllowlistSpec.Restrict)
	require.Equal(t, []string{"read"}, got.RunAllowlistSpec.AllowTools)

	require.NotNil(t, got.CallPolicy, "call policy baseline must not be dropped")
	require.Equal(t, session.CallOptionsSpecVersion, got.CallPolicy.Version)
	require.True(t, got.CallPolicy.TimeoutOptionsSet)
	require.Equal(t, 7*time.Minute, got.CallPolicy.TimeoutHardCap)
}
