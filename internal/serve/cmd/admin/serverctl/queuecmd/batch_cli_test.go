package queuecmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/text/ktavcodec"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestBatchCLIShowIncompleteTreeRefusesTerminalShape(t *testing.T) {
	identity := session.BatchCLIIdentity{Owner: "owner", Host: "host", Root: "root", Runner: "runner"}
	terminal := session.BatchLiveNode{NodeID: "root", Kind: "batch", State: "completed"}
	cases := []struct {
		name  string
		level session.BatchLevelResult
	}{
		{name: "incomplete terminal-looking tree", level: session.BatchLevelResult{Node: terminal, Subtree: []session.BatchLiveNode{terminal}, Complete: false}},
		{name: "failed fetch", level: session.BatchLevelResult{Err: errors.New("source unavailable"), Node: terminal, Subtree: []session.BatchLiveNode{terminal}}},
		{name: "completed state without complete snapshot", level: session.BatchLevelResult{Node: terminal, Subtree: []session.BatchLiveNode{terminal}, Complete: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderBatchShow(identity, tc.level)
			require.EqualError(t, err, "batch tree is incomplete or host status unknown")
			require.Equal(t, batchShowTree{}, got)
		})
	}
}

func TestBatchSpecParsesJSONAndKtavFilesAndStdin(t *testing.T) {
	ktav := "mode: parallel\nitems: [\n    {\n        tool: view\n        input: {\n            path: x\n        }\n    }\n]\n"
	jsonInput := `{"mode":"parallel","items":[{"tool":"view","input":{"path":"x"}}]}`
	jsonTree, err := ktavcodec.ParseJSONLossless([]byte(jsonInput))
	require.NoError(t, err)
	ktavTree, err := ktavcodec.DecodeKtav([]byte(ktav))
	require.NoError(t, err)
	require.NoError(t, ktavcodec.Equivalent(jsonTree, ktavTree))
	write := func(dir, name, body string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name, filename, body string
		stdin                bool
	}{
		{"json file", "spec.json", jsonInput, false},
		{"ktav file", "spec.ktav", ktav, false},
		{"json stdin", "-", jsonInput, true},
		{"ktav stdin", "-", ktav, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.filename
			if !tc.stdin {
				name = write(dir, tc.filename, tc.body)
			}
			cmd := &cobra.Command{}
			cmd.Flags().String("spec", name, "")
			if tc.stdin {
				cmd.SetIn(strings.NewReader(tc.body))
			}
			spec, err := parseBatchSpec(cmd)
			require.NoError(t, err)
			require.NotNil(t, spec.Batch)
			require.Len(t, spec.Batch.Items, 1)
			require.NotNil(t, spec.Batch.Items[0].Task)
			require.JSONEq(t, `{"tool":"view","input":{"path":"x"}}`, string(spec.Batch.Items[0].Task.Payload))
		})
	}
}

func TestBatchSpecStdinDetectsKtavCommentsQuotedKeysAndRootObject(t *testing.T) {
	for _, raw := range []string{
		"## leading comment\nmode: parallel\nitems: [{tool: view, input: {path: x}}]\n",
		`"mode": parallel` + "\n" + `"items": [{"tool": view, "input": {"path": x}}]` + "\n",
		"{ mode: parallel, items: [{ tool: view, input: { path: x } }] }\n",
	} {
		cmd := &cobra.Command{}
		cmd.Flags().String("spec", "-", "")
		cmd.SetIn(strings.NewReader(raw))
		spec, err := parseBatchSpec(cmd)
		require.NoError(t, err, raw)
		require.NotNil(t, spec.Batch, raw)
		require.Len(t, spec.Batch.Items, 1, raw)
		require.NotNil(t, spec.Batch.Items[0].Task, raw)
	}
}

func TestBatchSpecRejectsMalformedKtavFileAndStdin(t *testing.T) {
	for _, tc := range []struct{ name, spec, body string }{
		{"file", "bad.ktav", "mode: [oops\n"},
		{"stdin", "-", "mode: [oops\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := tc.spec
			if name != "-" {
				name = filepath.Join(t.TempDir(), name)
				require.NoError(t, os.WriteFile(name, []byte(tc.body), 0o600))
			}
			cmd := &cobra.Command{}
			cmd.Flags().String("spec", name, "")
			if tc.spec == "-" {
				cmd.SetIn(strings.NewReader(tc.body))
			}
			_, err := parseBatchSpec(cmd)
			require.Error(t, err)
			require.Contains(t, strings.ToLower(err.Error()), "ktav decode")
		})
	}
}

func TestBatchSpecRejectsUnknownTrailingAndMixedFields(t *testing.T) {
	cases := []struct{ name, raw, contains string }{{"unknown", `{"mode":"parallel","unknown":1}`, "unknown field"}, {"trailing", `{"tool":"view","input":{}} {}`, "trailing JSON value"}, {"mixed", `{"tool":"view","mode":"parallel"}`, "task and batch fields cannot be mixed"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("spec", "-", "")
			cmd.SetIn(strings.NewReader(tc.raw))
			_, err := parseBatchSpec(cmd)
			require.ErrorContains(t, err, tc.contains)
		})
	}
}

func TestBatchSpecDefaultsAndNestedValidInput(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("spec", "-", "")
	cmd.SetIn(strings.NewReader(`{"items":[{"tool":"view","input":{"path":"x"}},{"mode":"sequential","items":[{"tool":"edit","input":{}}]}]}`))
	spec, err := parseBatchSpec(cmd)
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Equal(t, jobbatch.ModeParallel, spec.Batch.Mode)
	require.Empty(t, spec.Batch.OnFail, "parallel on_fail must stay empty; the engine owns parallel policy")
	require.Equal(t, jobbatch.NotifyAll, spec.Batch.Notify)
	require.Len(t, spec.Batch.Items, 2)
	require.NotNil(t, spec.Batch.Items[1].Batch)
	require.Equal(t, jobbatch.ModeSequential, spec.Batch.Items[1].Batch.Mode)
	require.Equal(t, jobbatch.OnFailStop, spec.Batch.Items[1].Batch.OnFail, "sequential on_fail defaults to stop")
	for i := range spec.Batch.Items {
		if spec.Batch.Items[i].Task != nil {
			spec.Batch.Items[i].Task.Kind = "test"
		}
		if spec.Batch.Items[i].Batch != nil {
			for j := range spec.Batch.Items[i].Batch.Items {
				if spec.Batch.Items[i].Batch.Items[j].Task == nil {
					t.Fatal("test fixture expected a nested leaf task")
				}
				spec.Batch.Items[i].Batch.Items[j].Task.Kind = "test"
			}
		}
	}
	engine, err := jobbatch.NewEngine(jobbatch.Limits{MaxConcurrent: 2, MaxDepth: 8, MaxNodes: 16})
	require.NoError(t, err)
	transition, err := engine.Apply(jobbatch.Event{Type: jobbatch.EventSubmit, Root: "parsed-cli-root", Spec: spec})
	require.NoError(t, err)
	require.Len(t, transition.Changed, 4)
}

func TestBatchSpecRejectsParallelOnFailIncludingNested(t *testing.T) {
	for _, raw := range []string{
		`{"mode":"parallel","on_fail":"stop","items":[{"tool":"view","input":{}}]}`,
		`{"items":[{"mode":"parallel","on_fail":"continue","items":[{"tool":"view","input":{}}]}]}`,
	} {
		cmd := &cobra.Command{}
		cmd.Flags().String("spec", "-", "")
		cmd.SetIn(strings.NewReader(raw))
		_, err := parseBatchSpec(cmd)
		require.ErrorContains(t, err, "on_fail is only valid for sequential batches")
	}
}

func TestBatchSpecSequentialExplicitOnFailValues(t *testing.T) {
	for _, onFail := range []string{"stop", "continue"} {
		cmd := &cobra.Command{}
		cmd.Flags().String("spec", "-", "")
		cmd.SetIn(strings.NewReader(fmt.Sprintf(`{"mode":"sequential","on_fail":%q,"items":[{"tool":"view","input":{}}]}`, onFail)))
		spec, err := parseBatchSpec(cmd)
		require.NoError(t, err)
		require.Equal(t, jobbatch.OnFail(onFail), spec.Batch.OnFail)
	}
}
