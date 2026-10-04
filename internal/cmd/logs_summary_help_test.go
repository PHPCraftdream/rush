package cmd

// Help text of `rush logs summary` and of its parent. The command sells
// itself as strictly read-only, so that promise has to be in the help an
// operator reads -- and in the list the parent prints.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestLogsSummaryHelp_StatesTheReadOnlyContract: the help must carry the
// promise the implementation keeps (opened for reading only, nothing written,
// truncated or removed, no database) and document every flag the command
// registers.
//
// Revert-check: with the "no database is opened" or the "not created" clause
// removed from Long, this test fails naming them.
func TestLogsSummaryHelp_StatesTheReadOnlyContract(t *testing.T) {
	t.Parallel()

	long := oneLine(logsSummaryCmd.Long)
	require.Contains(t, long, "read-only", "the command's core contract must be in the help")
	require.Contains(t, long, "not created", "the help must say a missing file is reported, not created")
	require.Contains(t, long, "no database", "the help must say no database is opened")

	require.Equal(t, "summary", logsSummaryCmd.Use)
	require.Contains(t, oneLine(logsSummaryCmd.Short), "read-only")

	for _, name := range []string{"since", "file", "gap", "json"} {
		require.NotNilf(t, logsSummaryCmd.Flags().Lookup(name), "flag --%s is missing from the help", name)
	}
	require.Equal(t, defaultSilenceGap, 15*time.Minute)

	example := oneLine(logsSummaryCmd.Example)
	require.Contains(t, example, "--file")
	require.Contains(t, example, "--since")
	require.Contains(t, example, "--json")

	// --data-dir and --cwd are the root command's persistent flags: they are
	// documented, not re-registered here.
	require.Nil(t, logsSummaryCmd.PersistentFlags().Lookup("data-dir"),
		"--data-dir must not be re-registered on the subcommand")
}

// TestLogsSummaryHelp_IrregularLinesAreCountedNotFatal: the help must say a
// non-JSON line is counted, not skipped and not fatal -- that is the
// difference between a summary over a whole log and one that dies on the
// first stack trace.
func TestLogsSummaryHelp_IrregularLinesAreCountedNotFatal(t *testing.T) {
	t.Parallel()

	long := oneLine(logsSummaryCmd.Long)
	require.Contains(t, long, "counted as irregular")
	require.Contains(t, long, "not JSON")
	require.Contains(t, long, "(none)", "an empty section must print (none)")
}

// TestLogsSummaryHelp_ListsEverySection: the help enumerates the three
// questions the summary answers, because an operator reads the help before
// deciding whether the command covers what they are looking for.
func TestLogsSummaryHelp_ListsEverySection(t *testing.T) {
	t.Parallel()

	long := oneLine(logsSummaryCmd.Long)
	require.Contains(t, long, "WARN and ERROR")
	require.Contains(t, long, "process start")
	require.Contains(t, long, "silence gaps")
}

// TestLogsParentHelp_ListsSummary: the `logs` help lists every subcommand it
// has, and summary must be registered on the command itself -- one that is
// missing from it does not exist for the operator reading it.
//
// Revert-check: removing summary from the parent Long, or from AddCommand,
// turns this red.
func TestLogsParentHelp_ListsSummary(t *testing.T) {
	t.Parallel()

	long := oneLine(logsCmd.Long)
	require.Contains(t, long, "summary")
	require.Contains(t, long, "strictly read-only", "the parent must carry the summary's contract")

	var registered bool
	for _, sub := range logsCmd.Commands() {
		if sub.Name() == "summary" {
			registered = true
			require.Same(t, logsSummaryCmd, sub, "the registered command must be the one under test")
		}
	}
	require.True(t, registered, "`logs summary` must be registered on the logs command")
}
