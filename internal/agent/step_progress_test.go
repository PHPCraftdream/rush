package agent

import (
	"errors"
	"math"
	"testing"

	"charm.land/fantasy"

	"github.com/stretchr/testify/require"
)

// maxIntEnd is the "to the end of the file" end of a readSpan.
const maxInt = math.MaxInt

// asyncCapTag is the ClientMetadata async_tool.go stamps on a workLedger.Start
// the session's cap refused: the job never started, so nothing ran and no
// claim was taken (work_ledger_cap.go).
const asyncCapTag = `{"async_cap":{"running":50,"limit":50,"wait_timers":30}}`

// The classifier of docs/plans/2026-10-01-in-turn-progress-guard.md §5.1 (T1):
// one table, the cases the in-turn guard and #1113 both depend on.
//
// Revert-check: dropping the "wait needs async metadata" gate below turns the
// SDK `echo x` line red (a sync sleep blocks, and #1113 always counted it
// act); dropping read coverage turns the covered `view` line red; without the
// async_cap branch a cap-refused start is an act -- progress on a step where
// the tool never ran -- and the three async-cap lines below are red.
func TestClassifyStepCall(t *testing.T) {
	t.Parallel()

	startedTag := `{"async":true,"job_id":"j","status":"running","claim_id":"claim-X"}`
	call := func(name, input string) fantasy.ToolCallContent {
		return fantasy.ToolCallContent{ToolCallID: "c1", ToolName: name, Input: input}
	}
	ok := func(meta string) fantasy.ToolResultContent {
		return fantasy.ToolResultContent{
			ToolCallID:     "c1",
			Result:         fantasy.ToolResultOutputContentText{Text: "done"},
			ClientMetadata: meta,
		}
	}
	errored := func() fantasy.ToolResultContent {
		return fantasy.ToolResultContent{
			ToolCallID: "c1",
			Result:     fantasy.ToolResultOutputContentError{Error: errors.New("old_string not found")},
		}
	}
	unknownTool := func() fantasy.ToolResultContent {
		return fantasy.ToolResultContent{
			ToolCallID: "c1",
			Result:     fantasy.ToolResultOutputContentError{Error: errors.New("tool not found: mcp_foo")},
		}
	}
	guardRefusedResult := func() fantasy.ToolResultContent {
		return fantasy.ToolResultContent{
			ToolCallID:     "c1",
			Result:         fantasy.ToolResultOutputContentText{Text: "refused"},
			ClientMetadata: `{"async":true,"progress_guard":{"refused":"wait"}}`,
		}
	}
	capRefusal := func() fantasy.ToolResultContent {
		return fantasy.ToolResultContent{
			ToolCallID: "c1",
			Result: fantasy.ToolResultOutputContentError{
				Error: errors.New("Refused: this job was not started — this session already has 50 running async jobs (limit 50)."),
			},
			ClientMetadata: asyncCapTag,
		}
	}

	const viewPath = `C:\repo\main.go`
	viewInput := `{"file_path":"C:\\repo\\main.go","offset":0,"limit":163}`
	partial := readCoverage{}
	partial.add(viewPath, readSpan{start: 0, end: 80})
	full := readCoverage{}
	full.add(viewPath, readSpan{start: 0, end: 163})

	fsCovered := readCoverage{}
	fsCovered.add("a.go", readSpan{start: 0, end: 10})
	fsCovered.add("b.go", readSpan{start: 5, end: 7})
	fsInput := `{"items":[{"path":"a.go","start_line":1,"end_line":10},{"path":"b.go","line":6,"radius":0}]}`

	cases := []struct {
		name  string
		call  fantasy.ToolCallContent
		res   fantasy.ToolResultContent
		seen  readCoverage
		class stepCallClass
	}{
		{"bash echo async -> wait", call("bash", `{"command":"echo x"}`), ok(startedTag), nil, stepCallWait},
		{"bash echo sdk -> act", call("bash", `{"command":"echo x"}`), ok(""), nil, stepCallAct},
		{"bash compound -> act", call("bash", `{"command":"sleep 5 && go test"}`), ok(startedTag), nil, stepCallAct},
		{"bash redirect -> act", call("bash", `{"command":"echo x > f"}`), ok(startedTag), nil, stepCallAct},
		{"run_command sleep -> wait", call("run_command", `{"program":"sleep","args":["5"]}`), ok(startedTag), nil, stepCallWait},
		{"run_command sleep sdk -> act", call("run_command", `{"program":"sleep","args":["5"]}`), ok(""), nil, stepCallAct},
		{"bash sleep sdk -> act", call("bash", `{"command":"sleep 1"}`), ok(""), nil, stepCallAct},
		{"view fresh -> read", call("view", viewInput), ok(""), nil, stepCallRead},
		{"view seen nil -> read", call("view", viewInput), ok(""), readCoverage{}, stepCallRead},
		{"view covered -> reread", call("view", viewInput), ok(""), full, stepCallReread},
		{"view partial -> read", call("view", viewInput), ok(""), partial, stepCallRead},
		{"view default limit covered -> reread", call("view", `{"file_path":"C:\\repo\\main.go"}`), ok(""), wholeFileCoverage(viewPath), stepCallReread},
		{"fs_read all covered -> reread", call("fs_read", fsInput), ok(""), fsCovered, stepCallReread},
		{"fs_read one window open -> read", call("fs_read", fsInput), ok(""), readCoverage{}, stepCallRead},
		{"fs_read whole file covered -> reread", call("fs_read", `{"items":[{"path":"c.go"}]}`), ok(""), wholeFileCoverage("c.go"), stepCallReread},
		{"fs_read unparsable -> read", call("fs_read", `{"items":`), ok(""), full, stepCallRead},
		{"grep -> read", call("grep", `{"pattern":"x","path":"."}`), ok(""), nil, stepCallRead},
		{"job_output -> neutral", call("job_output", `{"job_id":"j"}`), ok(""), nil, stepCallNeutral},
		{"todos -> neutral", call("todos", `{"todos":[]}`), ok(""), nil, stepCallNeutral},
		{"edit error -> act", call("edit", `{"file_path":"a.go"}`), errored(), nil, stepCallAct},
		{"unknown mcp -> act", call("mcp_foo", `{}`), ok(""), nil, stepCallAct},
		{"tool not found -> neutral", call("mcp_foo", `{}`), unknownTool(), nil, stepCallNeutral},
		{"guard refusal -> refused", call("view", viewInput), guardRefusedResult(), full, stepCallRefused},
		{"refusal outranks neutral tool", call("job_output", `{}`), guardRefusedResult(), nil, stepCallRefused},
		{"async cap refusal -> refused", call("bash", `{"command":"go build ./..."}`), capRefusal(), nil, stepCallRefused},
		{"async cap refusal of a wait command -> refused", call("bash", `{"command":"cd /tmp && sleep 600; echo w1"}`), capRefusal(), nil, stepCallRefused},
		{"async cap refusal alongside a read -> refused (not read)", call("view", viewInput), capRefusal(), full, stepCallRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.class.String(), classifyStepCall(tc.call, tc.res, stepClassifyCtx{seen: tc.seen}).String())
		})
	}
}

// wholeFileCoverage is the coverage an earlier full-file read leaves behind.
func wholeFileCoverage(path string) readCoverage {
	c := readCoverage{}
	c.add(path, readSpan{start: 0, end: maxInt})
	return c
}

// TestReadCoverageUnit pins the interval algebra the reread class rests on.
func TestReadCoverageUnit(t *testing.T) {
	t.Parallel()

	c := readCoverage{}
	c.add("a.go", readSpan{start: 5, end: 9})
	c.add("a.go", readSpan{start: 0, end: 3}) // out of order, before the first
	require.Equal(t, []readSpan{{start: 0, end: 3}, {start: 5, end: 9}}, c["a.go"], "add keeps windows sorted")
	c.add("a.go", readSpan{start: 2, end: 6}) // bridges the two
	require.Equal(t, []readSpan{{start: 0, end: 9}}, c["a.go"], "touching windows merge")

	require.True(t, c.covered("a.go", readSpan{start: 0, end: 9}))
	require.True(t, c.covered("a.go", readSpan{start: 6, end: 7}))
	require.False(t, c.covered("a.go", readSpan{start: 0, end: 10}))
	require.False(t, c.covered("a.go", readSpan{start: 9, end: 11}), "a window past the covered range is a gap")
	require.False(t, c.covered("b.go", readSpan{start: 0, end: 9}))
	require.True(t, c.covered("a.go", readSpan{start: 3, end: 3}), "an empty window is trivially covered")

	snap := c.snapshot()
	c.add("a.go", readSpan{start: 100, end: 200})
	require.Len(t, snap["a.go"], 1, "snapshot is a deep copy")
	c.clear()
	require.Empty(t, c)
}
