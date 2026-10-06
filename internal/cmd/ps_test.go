// Tests for `rush ps`. Every snapshot file is produced by the heartbeat
// package's own writer API (RecordRequest/AddUsage/EndSession) so a format
// drift breaks these tests; files are only ever mutated afterwards by
// decode->edit->re-encode of a writer-produced file (never hand-written JSON).
//
// Revert-checks (one single-line production change each; the orchestrator
// runs the mutants, never this file):
//
//	TestPsEmptyDir: remove the len(rows)==0 early return in psFrameText —
//	  the table header would print instead of "no active rush processes".
//	TestPsNoRushDirCreated: make psRunE go through config.Load/createDotRushDir
//	  (or setupAppLite) — a .rush directory would appear in the cwd.
//	TestPsOneLiveRow: drop heartbeat.SetDirFunc(psHeartbeatDir) in psRunE —
//	  ReadAll would resolve an unset dir and no row would render.
//	TestPsStoppedAndStaleHiddenByDefault: make psRowVisibleByDefault return
//	  true for stopped rows unconditionally — the 20-minute-old stopped row
//	  and the stale row would leak into the default view.
//	TestPsStoppedAndStaleShownWithAll: drop the !showAll branch in
//	  filterPsRows — --all would show nothing extra.
//	TestPsIdleAfterTwoMinutesQuiet: change psIdleAfterSec below the seeded
//	  quiet period — the row would render as running instead of idle.
//	TestPsModelFilterIsCaseInsensitive: drop strings.ToLower in
//	  psRowMatchesModel — the uppercase filter would match nothing.
//	TestPsJSONStableKeys: rename any psJSONDoc/psJSONEntry json tag — the
//	  decode of the stable keys would fail.
//	TestPsStaleModelCauseSessionOverride: make psStaleCause return
//	  psCauseSlotAtStart unconditionally — the wrong cause would render.
//	TestPsStaleModelCauseSlotAtStart: break psSlotAtStartFor's value match —
//	  the cause would degrade to session-override.
//	TestPsStaleModelCausePerCall: remove the len(sources) branch in
//	  psStaleCause — the cause would degrade to session-override.
//	TestPsStaleModelAbsentWhenSlotMatches: remove the current-slot skip in
//	  psStaleModels — matching rows would wrongly flag.
//	TestPsPurposeLinesOnlyWhenMultiple: drop the len(r.purposes)>1 guard in
//	  psFrameText — single-purpose rows would print a split line.
//	TestPsFooterSumsRows: make psSummaryRows aggregate a single row (e.g.
//	  rows[:1]) — the per-model totals would stop summing.
//	TestPsSlotsUnreadableStillSucceeds: make psRunE fail when
//	  readPsSlots returns known=false — the command would error instead of
//	  degrading to "slots unknown".
//	TestPsWatchFrameDirect: break psFrameText (drop the footer or the
//	  STALE-MODEL column) — the single rendered frame would be incomplete.
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// psIsolateEnv points all three real global paths at throwaway directories
// (heartbeat dir, global data, global config) and chdirs into a fresh
// workspace-less temp dir so `rush ps` must work without either.
func psIsolateEnv(t *testing.T) (hbDir, dataDir string) {
	t.Helper()
	tmp := t.TempDir()
	hbDir = filepath.Join(tmp, "heartbeat")
	dataDir = filepath.Join(tmp, "data")
	require.NoError(t, os.MkdirAll(hbDir, 0o755))
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	t.Setenv("RUSH_HEARTBEAT_DIR", hbDir)
	heartbeat.ResetForTest()
	t.Cleanup(heartbeat.ResetForTest)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(tmp, "config"))
	t.Chdir(t.TempDir())
	return hbDir, dataDir
}

// psCtx builds attribution for a unique root session. An empty source keeps
// the model's Sources list empty (session-override territory).
func psCtx(session, source string, purpose heartbeat.Purpose) context.Context {
	return heartbeat.WithContext(context.Background(), heartbeat.Context{
		RootSessionID: session,
		Purpose:       purpose,
		Role:          "smart",
		Source:        source,
	})
}

// psSeedLiveRow records one request plus usage for the session and waits
// for the writer's background flush to publish the snapshot.
func psSeedLiveRow(t *testing.T, hbDir, session, provider, model, source string, purpose heartbeat.Purpose) string {
	t.Helper()
	ctx := psCtx(session, source, purpose)
	heartbeat.RecordRequest(ctx, provider, model, nil)
	heartbeat.AddUsage(ctx, provider, model, heartbeat.Usage{Input: 1000, Output: 250, CostUSD: 0.01})
	return psSnapshotForSession(t, hbDir, session)
}

// psSeedStoppedRow records a request then ends the session, which marks the
// row stopped and force-flushes it.
func psSeedStoppedRow(t *testing.T, hbDir, session, provider, model string) string {
	t.Helper()
	ctx := psCtx(session, "", heartbeat.PurposeTurn)
	heartbeat.RecordRequest(ctx, provider, model, nil)
	heartbeat.EndSession(session)
	return psSnapshotForSession(t, hbDir, session)
}

// psSnapshotForSession returns the snapshot path whose session field equals
// session, so later rewrites never touch another row's file.
func psSnapshotForSession(t *testing.T, dir, session string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		for _, f := range entries {
			name := f.Name()
			if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(dir, name))
			if rerr != nil {
				continue
			}
			var doc struct {
				Session string `json:"session"`
			}
			if json.Unmarshal(data, &doc) == nil && doc.Session == session {
				return filepath.Join(dir, name)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the snapshot of session %s in %s", session, dir)
	return ""
}

// psWaitRequests waits until the session's published snapshot counts n
// requests: the writer flushes asynchronously, ps reads only files.
func psWaitRequests(t *testing.T, hbDir, session string, n int64) {
	t.Helper()
	path := psSnapshotForSession(t, hbDir, session)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var doc struct {
			Totals struct {
				Requests int64 `json:"requests"`
			} `json:"totals"`
		}
		if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &doc) == nil && doc.Totals.Requests >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d requests in the snapshot of %s", n, session)
}

// psRewriteEntry decodes a writer-produced snapshot, applies mutate, and
// re-encodes it in place — so everything except the mutated keys stays in
// the package's real on-disk format.
func psRewriteEntry(t *testing.T, path string, mutate func(doc map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	mutate(doc)
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o600))
}

// psWriteSettings seeds the global settings file with the given slots.
func psWriteSettings(t *testing.T, dataDir string, slots map[string]psSlotModel) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"models": slots})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), b, 0o644))
}

// resetPsFlags resets the shared psCmd flag values AND their Changed state
// between runs (cobra commands are package-level vars).
func resetPsFlags(t *testing.T) {
	t.Helper()
	for _, fl := range []string{"watch", "json", "model", "all"} {
		if f := psCmd.Flags().Lookup(fl); f != nil {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		}
	}
	psCmd.SetArgs(nil)
}

// runPsCmd drives `rush ps` through the real root command with stdout
// captured, resetting flags around the run.
func runPsCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	resetPsFlags(t)
	var buf bytes.Buffer
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		resetPsFlags(t)
	})
	rootCmd.SetArgs(append([]string{"ps"}, args...))
	runErr := rootCmd.Execute()
	_ = w.Close()
	os.Stdout = oldOut
	<-done
	return buf.String(), runErr
}

func TestPsEmptyDir(t *testing.T) {
	psIsolateEnv(t)
	out, err := runPsCmd(t)
	require.NoError(t, err)
	assert.Contains(t, out, "no active rush processes")
}

// TestPsNoRushDirCreated proves `rush ps` opens neither the sessions DB nor
// the workspace store: run from a bare cwd, it must leave no .rush behind.
func TestPsNoRushDirCreated(t *testing.T) {
	psIsolateEnv(t)
	_, err := runPsCmd(t)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(mustCwd(t), ".rush"))
	require.True(t, os.IsNotExist(statErr), "ps must not create a .rush directory in the cwd")
}

func mustCwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return wd
}

func TestPsOneLiveRow(t *testing.T) {
	hbDir, _ := psIsolateEnv(t)
	psSeedLiveRow(t, hbDir, "ps-t-live1", "prov", "m-live", "", heartbeat.PurposeTurn)
	out, err := runPsCmd(t)
	require.NoError(t, err)
	assert.Contains(t, out, "running")
	assert.Contains(t, out, "m-live")
	assert.Contains(t, out, "ps-t-liv")
	assert.Contains(t, out, "MODELS (shown rows)")
	assert.Contains(t, out, "SLOTS: unknown")
	assert.NotContains(t, out, "slot@start")
}

func TestPsStoppedAndStaleHiddenByDefault(t *testing.T) {
	hbDir, _ := psIsolateEnv(t)
	psSeedLiveRow(t, hbDir, "ps-t-a-live", "prov", "m-alpha", "", heartbeat.PurposeTurn)
	psSeedStoppedRow(t, hbDir, "ps-t-b-stop", "prov", "m-beta")
	oldStop := psSeedStoppedRow(t, hbDir, "ps-t-c-old", "prov", "m-gamma")
	psRewriteEntry(t, oldStop, func(doc map[string]any) {
		doc["last_beat"] = time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	})
	stale := psSeedLiveRow(t, hbDir, "ps-t-d-stale", "prov", "m-delta", "", heartbeat.PurposeTurn)
	psRewriteEntry(t, stale, func(doc map[string]any) {
		doc["pid"] = float64(4000000000)
	})
	out, err := runPsCmd(t)
	require.NoError(t, err)
	assert.Contains(t, out, "m-alpha", "the live row must show by default")
	assert.Contains(t, out, "m-beta", "a freshly stopped row shows within the default window")
	assert.Contains(t, out, "2 row(s) hidden")
	assert.NotContains(t, out, "m-gamma", "a 20-minute-old stopped row must be hidden")
	assert.NotContains(t, out, "m-delta", "a stale row must be hidden")
}

func TestPsStoppedAndStaleShownWithAll(t *testing.T) {
	hbDir, _ := psIsolateEnv(t)
	psSeedLiveRow(t, hbDir, "ps-t-a2-live", "prov", "m-alpha2", "", heartbeat.PurposeTurn)
	psSeedStoppedRow(t, hbDir, "ps-t-b2-stop", "prov", "m-beta2")
	oldStop := psSeedStoppedRow(t, hbDir, "ps-t-c2-old", "prov", "m-gamma2")
	psRewriteEntry(t, oldStop, func(doc map[string]any) {
		doc["last_beat"] = time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	})
	stale := psSeedLiveRow(t, hbDir, "ps-t-d2-stale", "prov", "m-delta2", "", heartbeat.PurposeTurn)
	psRewriteEntry(t, stale, func(doc map[string]any) {
		doc["pid"] = float64(4000000000)
	})
	out, err := runPsCmd(t, "--all")
	require.NoError(t, err)
	for _, model := range []string{"m-alpha2", "m-beta2", "m-gamma2", "m-delta2"} {
		assert.Contains(t, out, model, "--all must show %s", model)
	}
	assert.Contains(t, out, "stale")
	assert.Contains(t, out, "stopped")
}

func TestPsIdleAfterTwoMinutesQuiet(t *testing.T) {
	hbDir, _ := psIsolateEnv(t)
	p := psSeedLiveRow(t, hbDir, "ps-t-idle", "prov", "m-idle", "", heartbeat.PurposeTurn)
	psRewriteEntry(t, p, func(doc map[string]any) {
		doc["last_request_at"] = time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	})
	out, err := runPsCmd(t, "--model", "m-idle")
	require.NoError(t, err)
	assert.Contains(t, out, "idle")
}

func TestPsModelFilterIsCaseInsensitive(t *testing.T) {
	hbDir, _ := psIsolateEnv(t)
	psSeedLiveRow(t, hbDir, "ps-t-f1", "prov", "m-alpha", "", heartbeat.PurposeTurn)
	psSeedLiveRow(t, hbDir, "ps-t-f2", "ant", "m-claude", "", heartbeat.PurposeTurn)
	out, err := runPsCmd(t, "--model", "M-AL")
	require.NoError(t, err)
	assert.Contains(t, out, "m-alpha")
	assert.NotContains(t, out, "m-claude", "the filtered-out row must not render")
}

func TestPsJSONStableKeys(t *testing.T) {
	hbDir, dataDir := psIsolateEnv(t)
	psSeedLiveRow(t, hbDir, "ps-t-json", "prov", "m-live", "", heartbeat.PurposeTurn)
	psWriteSettings(t, dataDir, map[string]psSlotModel{
		"smart": {Provider: "prov", Model: "m-live"},
	})
	out, err := runPsCmd(t, "--json")
	require.NoError(t, err)
	var doc struct {
		SlotsKnown bool           `json:"slots_known"`
		Slots      map[string]any `json:"slots"`
		Entries    []struct {
			PID        int    `json:"pid"`
			State      string `json:"state"`
			Session    string `json:"session"`
			BeatAgeSec int64  `json:"beat_age_sec"`
			Totals     struct {
				Requests int64 `json:"requests"`
			} `json:"totals"`
			Models []struct {
				Provider string         `json:"provider"`
				Model    string         `json:"model"`
				Purposes map[string]any `json:"purposes"`
			} `json:"models"`
			StaleModels []any `json:"stale_models"`
			Agents      []any `json:"agents"`
		} `json:"entries"`
		Summary []struct {
			Model    string `json:"model"`
			Live     int    `json:"live"`
			Requests int64  `json:"requests"`
			LastAt   string `json:"last_at"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.True(t, doc.SlotsKnown)
	require.Contains(t, doc.Slots, "smart")
	require.NotNil(t, doc.Slots["smart"])
	// Match this test's session explicitly: the heartbeat worker may have
	// re-flushed older tests' rows into this dir mid-run.
	var e *struct {
		PID        int    `json:"pid"`
		State      string `json:"state"`
		Session    string `json:"session"`
		BeatAgeSec int64  `json:"beat_age_sec"`
		Totals     struct {
			Requests int64 `json:"requests"`
		} `json:"totals"`
		Models []struct {
			Provider string         `json:"provider"`
			Model    string         `json:"model"`
			Purposes map[string]any `json:"purposes"`
		} `json:"models"`
		StaleModels []any `json:"stale_models"`
		Agents      []any `json:"agents"`
	}
	for i := range doc.Entries {
		if doc.Entries[i].Session == "ps-t-json" {
			require.Nil(t, e, "session must appear exactly once")
			e = &doc.Entries[i]
		}
	}
	require.NotNil(t, e, "this test's session must be present")
	assert.Equal(t, "running", e.State)
	assert.Equal(t, int64(1), e.Totals.Requests)
	require.Len(t, e.Models, 1)
	assert.Equal(t, "prov", e.Models[0].Provider)
	assert.Equal(t, "m-live", e.Models[0].Model)
	assert.Contains(t, e.Models[0].Purposes, "turn")
	assert.Empty(t, e.StaleModels, "the row's model matches the current smart slot")
	var sum *struct {
		Model    string `json:"model"`
		Live     int    `json:"live"`
		Requests int64  `json:"requests"`
		LastAt   string `json:"last_at"`
	}
	for i := range doc.Summary {
		if doc.Summary[i].Model == "prov/m-live" {
			require.Nil(t, sum, "model must appear exactly once in the summary")
			sum = &doc.Summary[i]
		}
	}
	require.NotNil(t, sum, "the model must be summarised")
	assert.Equal(t, 1, sum.Live)
	assert.Equal(t, int64(1), sum.Requests)
	assert.NotEmpty(t, sum.LastAt)
}

func TestPsStaleModelCauseSessionOverride(t *testing.T) {
	hbDir, dataDir := psIsolateEnv(t)
	psWriteSettings(t, dataDir, map[string]psSlotModel{
		"smart": {Provider: "prov", Model: "m-other"},
	})
	psSeedLiveRow(t, hbDir, "ps-t-ovr", "prov", "m-solo", "", heartbeat.PurposeTurn)
	// --model scopes the assertions to this test's row: the heartbeat worker
	// re-flushes earlier tests' rows into whatever dir is current.
	out, err := runPsCmd(t, "--model", "m-solo")
	require.NoError(t, err)
	assert.Contains(t, out, "session-override")
	assert.NotContains(t, out, "slot@start")
	assert.NotContains(t, out, "per-call")
}

func TestPsStaleModelCausePerCall(t *testing.T) {
	hbDir, dataDir := psIsolateEnv(t)
	psWriteSettings(t, dataDir, map[string]psSlotModel{
		"smart": {Provider: "prov", Model: "m-other"},
	})
	psSeedLiveRow(t, hbDir, "ps-t-pc", "prov", "m-pc", "title", heartbeat.PurposeTitle)
	out, err := runPsCmd(t, "--model", "m-pc")
	require.NoError(t, err)
	assert.Contains(t, out, "per-call")
	assert.NotContains(t, out, "slot@start")
	assert.NotContains(t, out, "session-override")
}

func TestPsStaleModelAbsentWhenSlotMatches(t *testing.T) {
	hbDir, dataDir := psIsolateEnv(t)
	psWriteSettings(t, dataDir, map[string]psSlotModel{
		"smart": {Provider: "prov", Model: "m-match"},
	})
	psSeedLiveRow(t, hbDir, "ps-t-match", "prov", "m-match", "", heartbeat.PurposeTurn)
	// A dead row with an odd model must not flag either: only live rows do.
	psSeedStoppedRow(t, hbDir, "ps-t-dead", "prov", "m-odd")
	out, err := runPsCmd(t, "--model", "m-match")
	require.NoError(t, err)
	assert.NotContains(t, out, "session-override")
	assert.NotContains(t, out, "slot@start")
	assert.NotContains(t, out, "per-call")
	out, err = runPsCmd(t, "--model", "m-odd")
	require.NoError(t, err)
	assert.NotContains(t, out, "session-override")
	assert.NotContains(t, out, "slot@start")
	assert.NotContains(t, out, "per-call")
}

func TestPsPurposeLinesOnlyWhenMultiple(t *testing.T) {
	hbDir, _ := psIsolateEnv(t)
	psSeedLiveRow(t, hbDir, "ps-t-p1", "prov", "m-one", "", heartbeat.PurposeTurn)
	out, err := runPsCmd(t, "--model", "m-one")
	require.NoError(t, err)
	assert.NotContains(t, out, "turn ", "a single-purpose row must not print a split line")

	psSeedLiveRow(t, hbDir, "ps-t-p2", "prov", "m-two", "title", heartbeat.PurposeTitle)
	heartbeat.RecordRequest(psCtx("ps-t-p2", "", heartbeat.PurposeTurn), "prov", "m-two", nil)
	psWaitRequests(t, hbDir, "ps-t-p2", 2)
	out, err = runPsCmd(t, "--model", "m-two")
	require.NoError(t, err)
	assert.Contains(t, out, "title 1req")
	assert.Contains(t, out, "turn 1req")
}

func TestPsFooterSumsRows(t *testing.T) {
	hbDir, _ := psIsolateEnv(t)
	psSeedLiveRow(t, hbDir, "ps-t-s1", "prov", "m-sum", "", heartbeat.PurposeTurn)
	psSeedLiveRow(t, hbDir, "ps-t-s2", "prov", "m-sum", "", heartbeat.PurposeTurn)
	out, err := runPsCmd(t)
	require.NoError(t, err)
	// Each row: 1 request and 1250 tokens → summed: 2 requests, 2.5k tokens.
	assert.Regexp(t, `(?m)^prov/m-sum\s+2\s+2\s`, out)
	assert.Contains(t, out, "2.5k")
}

func TestPsSlotsUnreadableStillSucceeds(t *testing.T) {
	psIsolateEnv(t)
	out, err := runPsCmd(t, "--json")
	require.NoError(t, err)
	var doc struct {
		SlotsKnown bool           `json:"slots_known"`
		Slots      map[string]any `json:"slots"`
		Entries    []any          `json:"entries"`
		Summary    []any          `json:"summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.False(t, doc.SlotsKnown, "a missing settings file must degrade to slots unknown")
	assert.Nil(t, doc.Slots)
	assert.NotNil(t, doc.Entries)
	assert.NotNil(t, doc.Summary)
}

func TestPsWatchFrameDirect(t *testing.T) {
	row := psRow{
		entry:      heartbeat.Entry{PID: 42, Session: "sess-1234", LaunchCwd: filepath.Join("machine", "dir")},
		state:      "running",
		beatAgeSec: 5,
		modelKey:   "prov/m-x",
		requests:   3,
		tokens:     1500,
		costUSD:    0.5,
		roles:      []string{"smart"},
		purposes: []psPurpose{
			{Name: "turn", Requests: 2, Tokens: 900, CostUSD: 0.3},
			{Name: "title", Requests: 1, Tokens: 600, CostUSD: 0.2},
		},
	}
	frame := psFrameText([]psRow{row}, 0, psSlots{known: true, bySlot: map[string]psSlotModel{
		"smart": {Provider: "prov", Model: "m-x"},
	}}, time.Now())
	assert.Regexp(t, `STATE\s+AGE\s+MODEL`, frame)
	assert.Contains(t, frame, "running")
	assert.Contains(t, frame, "prov/m-x")
	assert.Contains(t, frame, "title 1req 600 tok $0.2000", "two purposes must print the split lines")
	assert.Contains(t, frame, "MODELS (shown rows)")
	assert.Contains(t, frame, "SLOTS: smart=prov/m-x")
	assert.Contains(t, frame, "42")

	one := row
	one.purposes = row.purposes[:1]
	frame = psFrameText([]psRow{one}, 0, psSlots{}, time.Now())
	assert.NotContains(t, frame, "turn ", "a single-purpose row must not print a split line")
	assert.Contains(t, frame, "SLOTS: unknown")
}

// TestPsStaleModelCauseSlotAtStart seeds slots_at_start through the real
// writer path (heartbeat.Init) so the slot value format is the package's
// own. Kept LAST in this file: Init re-dirties every registry row, and
// those rows flush into whatever heartbeat dir is current, so earlier
// tests must not run after it (they scope with --model regardless).
func TestPsStaleModelCauseSlotAtStart(t *testing.T) {
	hbDir, dataDir := psIsolateEnv(t)
	heartbeat.Init("ps-t-ws", map[string]string{"smart": "prov/m-atstart"})
	psSeedLiveRow(t, hbDir, "ps-t-atstart", "prov", "m-atstart", "", heartbeat.PurposeTurn)
	psWriteSettings(t, dataDir, map[string]psSlotModel{
		"smart": {Provider: "prov", Model: "m-current"},
	})
	out, err := runPsCmd(t, "--model", "m-atstart")
	require.NoError(t, err)
	assert.Contains(t, out, "slot@start")
	assert.Contains(t, out, "smart:slot@start")
	assert.NotContains(t, out, "session-override")
}
