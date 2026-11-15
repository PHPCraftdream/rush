package shell

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"mvdan.cc/sh/v3/interp"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shexec"
)

func TestNormalizeXargsArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"attached short value", []string{"-n1", "echo"}, []string{"-n", "1", "echo"}, false},
		{"separate short value", []string{"-n", "1", "echo"}, []string{"-n", "1", "echo"}, false},
		{"clustered t and n", []string{"-tn2", "cmd"}, []string{"-t", "-n", "2", "cmd"}, false},
		{"t before separate n", []string{"-t", "-n3", "cmd"}, []string{"-t", "-n", "3", "cmd"}, false},
		{"long max-args equals", []string{"--max-args=3", "cmd"}, []string{"-n", "3", "cmd"}, false},
		{"long max-args separate", []string{"--max-args", "3", "cmd"}, []string{"-n", "3", "cmd"}, false},
		{"long null", []string{"--null", "cmd"}, []string{"-0", "cmd"}, false},
		{"long verbose", []string{"--verbose", "cmd"}, []string{"-t", "cmd"}, false},
		{"double dash ends options", []string{"--", "-n1"}, []string{"--", "-n1"}, false},
		{"boolean cluster", []string{"-0tp", "cmd"}, []string{"-0", "-t", "-p", "cmd"}, false},
		{"bare dash is command", []string{"-", "x"}, []string{"-", "x"}, false},
		{"command args untouched", []string{"echo", "-n1"}, []string{"echo", "-n1"}, false},
		{"unsupported -I untouched", []string{"-I{}", "echo"}, []string{"-I{}", "echo"}, false},
		{"unknown char in cluster leaves token", []string{"-txn2", "cmd"}, []string{"-txn2", "cmd"}, false},
		{"empty args", []string{}, []string{}, false},
		{"zero max args", []string{"-n0"}, nil, true},
		{"negative max args", []string{"-n", "-1"}, nil, true},
		{"missing value", []string{"-n"}, nil, true},
		{"non-numeric", []string{"-nabc"}, nil, true},
		{"n consumes rest of cluster", []string{"-tn2x", "cmd"}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeXargsArgs(tc.in)
			if tc.wantErr {
				require.ErrorIs(t, err, errInvalidXargsN)
				require.Equal(t, xargsInvalidNMessage, err.Error())
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// forceGoCoreUtils turns the Go coreutils on for one test (a Windows-only
// default) and skips where the handler does not build.
func forceGoCoreUtils(t *testing.T) {
	t.Helper()
	if shexec.CoreUtilsExecHandler == nil {
		t.Skip("Go coreutils handler unavailable on this platform")
	}
	orig := shexec.UseGoCoreUtils
	shexec.UseGoCoreUtils = true
	t.Cleanup(func() { shexec.UseGoCoreUtils = orig })
}

// REVERT CHECK: let u-root xargs start the COMMAND itself again (pass the
// normalized argv to next instead of running the batches) and these tests
// fail: the probe is not an executable, so only a batch that travels the
// exec chain can reach the recording handler.
func TestXargsRunsBatchesThroughExecChain(t *testing.T) {
	forceGoCoreUtils(t)
	var mu sync.Mutex
	var calls [][]string
	probe := func(_ context.Context, args []string) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, append([]string(nil), args...))
		return nil
	}
	stderr, err := runThroughProductionChain(t, map[string]interp.ExecHandlerFunc{"rush-xargs-probe": probe},
		`printf 'a b\nc\n' | xargs -t -n 1 rush-xargs-probe x`)
	require.NoError(t, err, stderr.String())
	require.Equal(t, [][]string{
		{"rush-xargs-probe", "x", "a"},
		{"rush-xargs-probe", "x", "b"},
		{"rush-xargs-probe", "x", "c"},
	}, calls)
	require.Equal(t, "rush-xargs-probe x a\nrush-xargs-probe x b\nrush-xargs-probe x c\n", stderr.String())
}

func TestXargsStopsAtFirstFailingBatch(t *testing.T) {
	forceGoCoreUtils(t)
	var mu sync.Mutex
	seen := 0
	probe := func(context.Context, []string) error {
		mu.Lock()
		defer mu.Unlock()
		seen++
		if seen == 2 {
			return interp.ExitStatus(3)
		}
		return nil
	}
	_, err := runThroughProductionChain(t, map[string]interp.ExecHandlerFunc{"rush-xargs-probe": probe},
		`printf 'a\nb\nc\n' | xargs -n 1 rush-xargs-probe`)
	require.Error(t, err)
	require.Equal(t, 3, ExitCode(err))
	require.Equal(t, 2, seen)
}

func TestPlanXargs(t *testing.T) {
	t.Parallel()

	plan, ok := planXargs([]string{"-t", "-n", "2", "-0", "cmd", "-n", "9"})
	require.True(t, ok)
	require.Equal(t, xargsPlan{maxArgs: 2, trace: true, null: true, command: []string{"cmd", "-n", "9"}}, plan)

	plan, ok = planXargs([]string{"--", "-x", "y"})
	require.True(t, ok)
	require.Equal(t, []string{"-x", "y"}, plan.command)
	require.Equal(t, xargsDefaultMaxArgs, plan.maxArgs)

	_, ok = planXargs([]string{"-p", "cmd"})
	require.False(t, ok, "the prompt stays with u-root")
	_, ok = planXargs([]string{"--unknown", "cmd"})
	require.False(t, ok, "an unknown option stays with u-root")
}

func TestReadXargsItems(t *testing.T) {
	t.Parallel()

	items, err := readXargsItems(strings.NewReader("a  b\n\n c\n"), false)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, items)

	items, err = readXargsItems(strings.NewReader("a b\x00c\x00tail"), true)
	require.NoError(t, err)
	require.Equal(t, []string{"a b", "c", "tail"}, items)

	items, err = readXargsItems(nil, false)
	require.NoError(t, err)
	require.Empty(t, items)
}
