package lockwrap

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestDecision(t *testing.T) {
	for _, tc := range []struct {
		pattern, command, name string
		wrap                   bool
	}{
		{"go build", "go build ./...", "go-build", true},
		{"exact:go test ./...", "go test ./...", "go-test", true},
		{"glob:go build *", "go build ./...", "go-build", true},
		{"go build", "go test ./...", "", false},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			wrap, name := Decision([]string{tc.pattern}, tc.command, false, false)
			require.Equal(t, tc.wrap, wrap)
			require.Equal(t, tc.name, name)
		})
	}
}

func TestLockWrapDecisionCompoundAndBackground(t *testing.T) {
	for _, command := range []string{"go build ./... && echo done", "go build ./... &"} {
		wrap, name := Decision([]string{"go build"}, command, false, false)
		require.False(t, wrap, command)
		require.Empty(t, name)
	}
	wrap, _ := Decision([]string{"go build"}, "go build ./...", true, false)
	require.False(t, wrap, "explicit background is not wrapped")
	wrap, _ = Decision([]string{"go build"}, "go build ./...", false, true)
	require.False(t, wrap, "retained sessions are not wrapped")
}

func TestRunPermissionsLockWrapJSON(t *testing.T) {
	var cfg config.Permissions
	require.NoError(t, json.Unmarshal([]byte(`{"run":{"lock_wrap":["go build","exact:go test ./...","glob:cargo test *"]}}`), &cfg))
	require.Equal(t, []string{"go build", "exact:go test ./...", "glob:cargo test *"}, cfg.Run.LockWrap)
}
