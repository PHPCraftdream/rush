package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

type acceptanceTestJournalWriter struct{ bytes.Buffer }

func (w *acceptanceTestJournalWriter) Close() error { return nil }
func acceptanceTestPtr[T any](v T) *T               { return &v }

// Classification is explicit: adding or removing a fixture requires a decision.
var acceptanceTestClasses = map[string]string{
	"c-a": "closed", "c-a-ii": "closed", "c-b": "closed", "c-b-mirror": "closed", "c-c": "closed",
	"c-d": "exit3-closed", "c-d-i": "closed", "c-d-ii": "exit3-closed", "c-d-iii": "exit3-closed",
	"c-e": "closed", "c-e-undeclared": "closed", "f-a": "closed", "f-b": "closed", "f-c": "closed-gap",
	"f-d": "closed", "f-e": "exit3-closed", "f-f": "closed", "l3": "closed",
	"r3-add": "closed", "r3-add-helper": "closed", "r3-chain": "closed", "r3-one": "closed",
	"r-a": "closed", "r-a-h-blocked": "closed", "r-a-h-held": "closed", "r-b": "closed",
	"r-c-empty": "closed", "r-c-pending": "closed", "r-d-i": "closed", "r-d-ii": "closed",
	"v-a-bad-ref": "invalid", "v-a-duplicate-ref": "invalid", "v-a-empty-ref": "invalid",
	"v-a-missing-ref": "invalid", "v-a-unknown-message": "invalid", "v-a-unknown-payload": "invalid",
	"v-b": "closed", "v-c": "closed", "v-c-empty-code": "closed", "v-c-kept-code": "closed",
	"v-c-result-code": "closed", "v-e-batch": "closed", "v-e-task": "closed",
	"w-a": "closed", "w-b-i": "closed", "w-b-ii": "closed", "w-c-i": "closed", "w-c-ii": "exit3-closed", "w-d": "closed",
}

func acceptanceTestLoad(t *testing.T, name string) *Scenario {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "scenarios", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := LoadScenario(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func acceptanceTestRun(t *testing.T, s *Scenario, gap bool) (RunResult, []byte, []jobbatch.Record) {
	t.Helper()
	w := &acceptanceTestJournalWriter{}
	var owned *RunHarness
	opts := RunOptions{OpenEvents: func() (io.WriteCloser, error) { return w, nil }, BeforeAction: func(_ context.Context, _ int, h *RunHarness) error { owned = h; return nil }}
	if gap {
		opts.BeforeEventsRead = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	}
	// Teardown is after the captured cut; it is not used as acceptance evidence.
	defer func() {
		if owned != nil {
			probe, cancel := context.WithCancel(context.Background())
			cancel()
			page, _ := owned.Runner.Events(probe, ^uint64(0))
			if page.Final == 0 {
				labTestJoinHarness(t, owned)
			}
		}
	}()
	got := RunScenario(context.Background(), s, opts)
	data := append([]byte(nil), w.Bytes()...)
	records := acceptanceTestRecords(t, data)
	acceptanceTestPrivacy(t, s, data)
	return got, data, records
}

func acceptanceTestRecords(t *testing.T, data []byte) []jobbatch.Record {
	t.Helper()
	var records []jobbatch.Record
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var tag struct {
			Type string `json:"type"`
		}
		if err = json.Unmarshal(raw, &tag); err != nil {
			t.Fatal(err)
		}
		if tag.Type == "header" || tag.Type == "footer" {
			continue
		}
		var r jobbatch.Record
		if err = json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

func acceptanceTestPrivacy(t *testing.T, s *Scenario, data []byte) {
	t.Helper()
	// Canonicalize journal objects so whitespace cannot conceal private objects.
	var public bytes.Buffer
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		acceptanceTestPrivateKeys(t, v)
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		public.Write(encoded)
	}
	reject := func(v any) {
		encoded, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(public.Bytes(), encoded) {
			t.Fatalf("journal leaked private value %s", encoded)
		}
	}
	var inspect func(any)
	inspect = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if len(x) > 0 {
				reject(x)
			}
			for _, child := range x {
				inspect(child)
			}
		case []any:
			if len(x) > 0 {
				reject(x)
			}
			for _, child := range x {
				inspect(child)
			}
		case string:
			// Short strings and public vocabulary (e.g. command/go) cannot prove a
			// privacy leak alone; their containing nonempty object is still checked.
			if len(x) >= 4 && !acceptanceTestPublicWord(x) {
				reject(x)
			}
		}
	}
	for _, m := range s.Messages {
		var v any
		if err := json.Unmarshal(m, &v); err != nil {
			t.Fatal(err)
		}
		inspect(v)
	}
	for _, p := range s.Payloads {
		encoded, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err = json.Unmarshal(encoded, &v); err != nil {
			t.Fatal(err)
		}
		inspect(v)
	}
	for _, sentinel := range []string{"PRIVATE_SENTINEL", "batchlab-error-sentinel", "acceptance-payload-secret", "acceptance-message-secret", "acceptance-argv-secret", "acceptance-summary-secret", "acceptance-details-secret"} {
		if bytes.Contains(data, []byte(sentinel)) {
			t.Fatalf("journal leaked %q", sentinel)
		}
		reject(sentinel)
	}
}

func acceptanceTestPublicWord(s string) bool {
	switch s {
	case "controlled", "process", "command", "completed", "failed", "cancelled", "interrupted", "timed_out", "pending", "running", "starting", "cancelling", "skipped", "stop", "print", "exit", "hold", "spawn":
		return true
	}
	return false
}

func acceptanceTestPrivateKeys(t *testing.T, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for key, child := range x {
			switch key {
			case "payload", "payloads", "message", "messages", "argv", "args", "program", "cwd", "summary", "details", "output", "error_text":
				t.Fatalf("journal leaked private field key %q", key)
			}
			acceptanceTestPrivateKeys(t, child)
		}
	case []any:
		for _, child := range x {
			acceptanceTestPrivateKeys(t, child)
		}
	}
}

func TestAcceptanceFixtureJournalReplaySweep(t *testing.T) {
	// L4/L6/E8/E9/R/C/F/V/W mutation: losing producer tails, settlements or private-data filtering breaks cross-slice journals.
	paths, err := filepath.Glob("testdata/scenarios/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 49 || len(acceptanceTestClasses) != 49 {
		t.Fatalf("fixture inventory=%d classifications=%d", len(paths), len(acceptanceTestClasses))
	}
	totals := map[string]int{}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		class, ok := acceptanceTestClasses[name]
		if !ok {
			t.Fatalf("unclassified %s", name)
		}
		totals[class]++
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				if class == "invalid" {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = LoadScenario(bytes.NewReader(data)); err == nil {
						t.Fatal("loader accepted invalid fixture")
					}
					var s Scenario
					if err = json.Unmarshal(data, &s); err != nil {
						t.Fatal(err)
					}
					opened := false
					got := RunScenario(context.Background(), &s, RunOptions{OpenEvents: func() (io.WriteCloser, error) { opened = true; return &acceptanceTestJournalWriter{}, nil }})
					if got.ExitCode != 2 || opened {
						t.Fatalf("invalid exit=%d opened=%t", got.ExitCode, opened)
					}
					return
				}
				s := acceptanceTestLoad(t, name)
				got, data, records := acceptanceTestRun(t, s, class == "closed-gap")
				want := *s.Expect.ExitCode
				if class == "closed-gap" {
					want = 5
				}
				if got.ExitCode != want {
					t.Fatalf("exit=%d want=%d mismatches=%v oracle=%s", got.ExitCode, want, got.Mismatches, got.OracleRule)
				}
				if strings.HasPrefix(class, "exit3") && want != 3 {
					t.Fatal("classification not declared exit3")
				}
				replay := Replay(s, bytes.NewReader(data))
				if class == "closed-gap" {
					f := got.Footer
					if f.Capture.Status != CaptureGap || f.Capture.FirstMissing == nil || *f.Capture.FirstMissing != 1 || f.Capture.ResumedAt == nil || len(records) != 4 || records[0].Cursor != *f.Capture.ResumedAt || replay.ExitCode != 5 {
						t.Fatalf("explicit gap failed: %+v replay=%d", f, replay.ExitCode)
					}
					acceptanceTestBoundary(t, f, records, false)
					t.Log("intentional F-c gap: retained suffix only; no invariant/history reconstruction")
					return
				}
				if got.Footer.Capture.Status != CaptureComplete {
					t.Fatalf("unexpected capture loss: %+v", got.Footer.Capture)
				}
				if class == "exit3-unfinished" {
					if got.Footer.Outcome.Status != OutcomeUnfinished || replay.ExitCode != 3 || got.Footer.FinalCursor != nil {
						t.Fatalf("unfinished cut: %+v replay=%d", got.Footer, replay.ExitCode)
					}
				} else {
					if got.Footer.Outcome.Status != OutcomeClosed {
						t.Fatalf("not closed: %+v", got.Footer)
					}
					acceptanceTestBoundary(t, got.Footer, records, true)
					wantReplay := 0
					for _, root := range got.Footer.Roots {
						if root.State != jobbatch.StateCompleted {
							wantReplay = 1
						}
					}
					if name == "c-e-undeclared" {
						// The declared exit-4 oracle remains a replay mismatch even
						// though the feed cannot reproduce the fixture phase error.
						wantReplay = ExitMismatch
					}
					if replay.ExitCode != wantReplay {
						t.Fatalf("closed replay=%d want=%d", replay.ExitCode, wantReplay)
					}
				}
				if !reflect.DeepEqual(got.Footer, replay.Footer) {
					t.Fatal("run/replay footer differs: outcome, roots/counts/settled_sequence or boundaries")
				}
				acceptanceTestPublicSweep(t, s, records, got.Footer.Outcome.Status == OutcomeClosed)
			})
		})
	}
	t.Logf("49 classified: %v", totals)
}

func acceptanceTestBoundary(t *testing.T, f JournalFooter, records []jobbatch.Record, complete bool) {
	t.Helper()
	if len(records) == 0 || f.FinalCursor == nil {
		t.Fatal("missing final boundary")
	}
	last := records[len(records)-1]
	if last.Type != jobbatch.RecordClosed || last.Cursor != *f.FinalCursor || f.LastCursor != *f.FinalCursor || last.Sequence != f.FinalSequence {
		t.Fatal("closed is not exact producer/writer boundary")
	}
	var previous uint64
	for i, r := range records {
		if r.Sequence < previous || r.Sequence == 0 {
			t.Fatal("nonmonotonic sequence")
		}
		previous = r.Sequence
		if complete && r.Cursor != uint64(i+1) {
			t.Fatal("cursor hole")
		}
		if i < len(records)-1 && r.Type == jobbatch.RecordClosed {
			t.Fatal("early closed")
		}
	}
	for _, root := range f.Roots {
		if root.SettledSequence == nil || *root.SettledSequence > f.FinalSequence || !root.State.Terminal() {
			t.Fatal("invalid settled root boundary")
		}
	}
}

// Reconstruct through public Apply/Changed only; never inspect Engine internals,
// snapshot history, private replay helpers, or consumer-selected notifications.
// Reconstruction depends on the production Engine.Apply implementation; it is
// not an independent scheduler oracle. Invariants below are separate assertions.
func acceptanceTestEvent(t *testing.T, s *Scenario, r jobbatch.Record) jobbatch.Event {
	t.Helper()
	e := jobbatch.Event{Type: jobbatch.EventType(r.Type)}
	switch r.Type {
	case jobbatch.RecordSubmit, jobbatch.RecordAdd:
		var action *Action
		for i := range s.Script {
			a := &s.Script[i]
			if a.Ref != nil && *a.Ref == r.Ref {
				action = a
				break
			}
		}
		if action == nil {
			t.Fatal("unknown action ref")
		}
		if r.Type == jobbatch.RecordSubmit {
			q, err := s.SubmitRequest(*action)
			if err != nil {
				t.Fatal(err)
			}
			e.Root, e.Spec, e.Ref = q.Root, q.Spec, q.Ref
		} else {
			q, err := s.AddRequest(*action)
			if err != nil {
				t.Fatal(err)
			}
			e.Parent, e.Items, e.Ref = q.Parent, q.Items, q.Ref
		}
	case jobbatch.RecordStopRequested:
		e.Node = r.Node
	case jobbatch.RecordStarted:
		e.Node, e.Token = r.Node, r.Token
	case jobbatch.RecordStartRejected:
		e.Node, e.Token, e.Reason, e.Code = r.Node, r.Token, r.Reason, r.Code
	case jobbatch.RecordSettled:
		e.Node, e.Token, e.Result = r.Node, r.Token, jobbatch.Result{State: r.State, Code: r.Code}
	case jobbatch.RecordControl:
		e.Node, e.Token, e.Op, e.Code = r.Node, r.Token, r.Op, r.Code
	case jobbatch.RecordDeadlineExpired:
		e.Node, e.Token = r.Node, r.Token
	case jobbatch.RecordCapacityAvailable:
		e.Group = r.Group
	case jobbatch.RecordRelease:
		e.Root = r.Root
	case jobbatch.RecordHostClosing:
	default:
		t.Fatalf("unexpected event %s", r.Type)
	}
	return e
}

func acceptanceTestPublicSweep(t *testing.T, s *Scenario, records []jobbatch.Record, closed bool) {
	t.Helper()
	engine, err := jobbatch.NewEngine(s.Limits)
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[jobbatch.ID]jobbatch.NodeView{}
	selected := map[jobbatch.ID]int{}
	finals := 0
	for i := 0; i < len(records); {
		eventRecord := records[i]
		end := i + 1
		for end < len(records) && records[end].Sequence == eventRecord.Sequence {
			end++
		}
		before := map[jobbatch.ID]jobbatch.State{}
		for id, v := range nodes {
			before[id] = v.State
		}
		tr, err := engine.Apply(acceptanceTestEvent(t, s, eventRecord))
		if err != nil {
			t.Fatalf("cursor %d public Apply: %v", eventRecord.Cursor, err)
		}
		if tr.Sequence != eventRecord.Sequence {
			t.Fatal("accepted event sequence mismatch")
		}
		for _, v := range tr.Changed {
			nodes[v.ID] = v
		}
		active := map[string]int{}
		total := 0
		for _, v := range nodes {
			if v.Kind == jobbatch.KindTask {
				if v.State.Active() {
					active[v.Group]++
					total++
				}
				continue
			}
			nonterminalChild := false
			for _, child := range nodes {
				if child.Parent == v.ID && !child.State.Terminal() {
					nonterminalChild = true
				}
			}
			if !v.State.Terminal() && !nonterminalChild {
				t.Fatalf("seq %d nonterminal batch %s without live direct child", tr.Sequence, v.ID)
			}
			if v.State.Terminal() {
				for _, desc := range nodes {
					for p := desc.Parent; p != ""; p = nodes[p].Parent {
						if p == v.ID && !desc.State.Terminal() {
							t.Fatalf("terminal %s above live %s", v.ID, desc.ID)
						}
					}
				}
			}
		}
		if total > s.Limits.MaxConcurrent {
			t.Fatal("global admission exceeded")
		}
		for g, n := range active {
			if cap, ok := s.Limits.ByGroup[g]; ok && n > cap {
				t.Fatal("group admission exceeded")
			}
		}
		noticed := map[jobbatch.ID]bool{}
		actual := []jobbatch.Notice{}
		commands := 0
		closedRecords := 0
		for _, r := range records[i+1 : end] {
			switch r.Type {
			case jobbatch.RecordCommand:
				commands++
				if r.Command == jobbatch.CommandStartLeaf {
					v := nodes[r.Node]
					if v.Kind != jobbatch.KindTask || !v.State.Active() {
						t.Fatal("start does not reserve active leaf")
					}
					for p := v.Parent; p != ""; p = nodes[p].Parent {
						if nodes[p].State.Terminal() {
							t.Fatal("start under terminal ancestor")
						}
					}
				}
			case jobbatch.RecordNotice:
				v, ok := nodes[r.Node]
				if !ok || !v.State.Terminal() || before[r.Node].Terminal() || selected[r.Node] != 0 {
					t.Fatalf("notice not unique settlement: %s seq %d", r.Node, tr.Sequence)
				}
				selected[r.Node]++
				noticed[r.Node] = true
				if r.Counts == nil {
					t.Fatal("notice missing counts")
				}
				actual = append(actual, jobbatch.Notice{Root: r.Root, Node: r.Node, Kind: r.Kind, State: r.State, Cause: r.Cause, Counts: *r.Counts})
			case jobbatch.RecordClosed:
				closedRecords++
			default:
				t.Fatal("multiple events in transition")
			}
		}
		if commands != len(tr.Commands) || !reflect.DeepEqual(actual, append([]jobbatch.Notice{}, tr.Notices...)) {
			t.Fatal("public commands/notices differ from transition")
		}
		for id := range noticed {
			v := nodes[id]
			if v.Kind == jobbatch.KindTask {
				continue
			}
			children := []jobbatch.ID{}
			for _, c := range nodes {
				if c.Parent == id {
					children = append(children, c.ID)
				}
			}
			if len(children) == 1 && noticed[children[0]] {
				t.Fatal("batch and sole child both selected")
			}
		}
		if tr.Final {
			finals++
			if closedRecords != 1 || end != len(records) {
				t.Fatal("Final not exact last closed transition")
			}
		} else if closedRecords != 0 {
			t.Fatal("closed without Final")
		}
		i = end
	}
	if closed {
		if finals != 1 {
			t.Fatal("missing unique Final")
		}
		for _, v := range nodes {
			if v.Parent == "" && selected[v.ID] != 1 {
				t.Fatalf("root notice count %s=%d", v.ID, selected[v.ID])
			}
		}
	} else if finals != 0 {
		t.Fatal("unfinished prefix became Final")
	}
}

func TestAcceptanceE1SameIdentityIngress(t *testing.T) {
	// E1/V-e mutation: wrapping nested leaves or changing OneItem policy changes the same-identity feed and normalized tree.
	synctest.Test(t, func(t *testing.T) {
		raw := acceptanceTestLoad(t, "v-e-task")
		explicit := acceptanceTestLoad(t, "v-e-task")
		explicit.Script[0].Ref = acceptanceTestPtr("acceptance-explicit")
		explicit.Script[0].Spec = &Spec{Batch: &BatchSpec{Mode: jobbatch.ModeParallel, Notify: acceptanceTestPtr(jobbatch.NotifyAll), Items: []Spec{*raw.Script[0].Spec}}}
		a, _, ar := acceptanceTestRun(t, raw, false)
		b, _, br := acceptanceTestRun(t, explicit, false)
		if a.ExitCode != 0 || b.ExitCode != 0 {
			t.Fatal("ingress run failed")
		}
		aq, err := raw.SubmitRequest(raw.Script[0])
		if err != nil {
			t.Fatal(err)
		}
		bq, err := explicit.SubmitRequest(explicit.Script[0])
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(aq.Spec, bq.Spec) || aq.Root != bq.Root {
			t.Fatal("normalized ordinary tree differs")
		}
		if len(ar) != len(br) {
			t.Fatal("same identity feed lengths differ")
		}
		launches, notices := 0, 0
		for i := range ar {
			if ar[i].Type == jobbatch.RecordSubmit {
				if ar[i].Ref == br[i].Ref {
					t.Fatal("refs not distinct")
				}
				ar[i].Ref = ""
				br[i].Ref = ""
			}
			if ar[i].Type == jobbatch.RecordCommand && ar[i].Command == jobbatch.CommandStartLeaf {
				launches++
			}
			if ar[i].Type == jobbatch.RecordNotice {
				notices++
			}
		}
		if launches != 1 || notices != 1 || !reflect.DeepEqual(ar, br) || !reflect.DeepEqual(a.Footer.Roots, b.Footer.Roots) {
			t.Fatal("same identity tree/launch/result/notices differ")
		}
	})
}

func TestAcceptanceDeterministicConstructionStreams(t *testing.T) {
	// E7/E8/R3/L4 mutation: unstable IDs, accepted-event sequence or notice construction changes fenced streams without retries.
	for _, name := range []string{"r-c-empty", "r3-one", "r3-chain", "r3-add", "r3-add-helper", "f-d", "v-e-task"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := acceptanceTestLoad(t, name)
				a, _, ar := acceptanceTestRun(t, s, false)
				b, _, br := acceptanceTestRun(t, s, false)
				if a.ExitCode != b.ExitCode || !reflect.DeepEqual(ar, br) || !reflect.DeepEqual(a.Footer, b.Footer) {
					t.Fatal("two fenced runs differ in cursor/sequence/type/IDs or metadata")
				}
				acceptanceTestPublicSweep(t, s, ar, true)
			})
		})
	}
}

func TestAcceptanceResultAndProcessPrivacyJournal(t *testing.T) {
	// E9/L1/L4 mutation: serializing launch payload/argv, Inject data or Result Summary/Details leaks secrets into a real journal.
	s := acceptanceTestLoad(t, "v-e-task")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := NewControlledExecutor()
	r, err := jobbatch.NewRunner(ctx, jobbatch.RunnerConfig{Limits: s.Limits, Executor: c, ControlTimeout: time.Second, RecordRetention: 128})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = r.Close(cleanup)
	}()
	spec := jobbatch.OneItem(jobbatch.TaskSpec{Kind: "controlled", Payload: json.RawMessage(`"acceptance-payload-secret"`)})
	b, err := r.Submit(ctx, jobbatch.SubmitRequest{Root: "T", Spec: spec, Ref: *s.Script[0].Ref})
	if err != nil {
		t.Fatal(err)
	}
	launch, err := c.AwaitStart(ctx, "T.1")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.ReleaseStart(ctx, launch.Token, StartDecision{Decision: DecisionAccept}); err != nil {
		t.Fatal(err)
	}
	acceptanceTestAwaitRecord(t, ctx, r, jobbatch.RecordStarted)
	if err = r.Inject(ctx, "T.1", json.RawMessage(`"acceptance-message-secret"`)); err != nil {
		t.Fatal(err)
	}
	msg, err := c.AwaitInject(ctx, launch.Token)
	if err != nil || string(msg) != `"acceptance-message-secret"` {
		t.Fatal("message exercise failed")
	}
	if err = c.Complete(ctx, launch.Token, jobbatch.Result{State: jobbatch.StateCompleted, Summary: "acceptance-summary-secret", Details: json.RawMessage(`"acceptance-details-secret"`)}); err != nil {
		t.Fatal(err)
	}
	summary, err := b.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view, err := r.View(ctx, "T.1")
	if err != nil || view.Outcome.Summary != "acceptance-summary-secret" {
		t.Fatal("real Summary did not reach inspection")
	}
	if err = r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := r.Events(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	w := &acceptanceTestJournalWriter{}
	capture := newJournalCapture(s, w, NewOracle())
	capture.batches = map[jobbatch.ID]*jobbatch.Batch{"T": b}
	capture.drain(ctx, r)
	f := capture.footer(s, []JournalInput{{Ref: *s.Script[0].Ref, Action: ActionSubmit, Result: InputAccepted, IDs: acceptanceTestPtr([]jobbatch.ID{"T"})}}, RequestCounts{})
	capture.writeFooter(f)
	if len(f.Roots) != 1 || f.Roots[0].Counts != summary.Counts || f.Roots[0].SettledSequence == nil || *f.Roots[0].SettledSequence != summary.Sequence {
		t.Fatal("production writer lost real result settlement")
	}
	acceptanceTestBoundary(t, f, capture.records, true)
	acceptanceTestPrivacy(t, s, w.Bytes())
	if got := Replay(s, bytes.NewReader(w.Bytes())); got.ExitCode != 0 {
		t.Fatalf("result journal replay=%d", got.ExitCode)
	}
	acceptanceTestPublicSweep(t, s, page.Records, true)
	// This uses the process owner's once-built helper under the outer capm run.
	program := processTestHelper(t)
	args := []string{"print", "acceptance-argv-secret"}
	p := &Scenario{Version: 1, Limits: s.Limits, Runner: s.Runner, WaitBoundMS: 10000, Payloads: map[string]Payload{"p": {Kind: "process", Program: &program, Args: &args}}, Script: []Action{{Action: ActionSubmit, Root: acceptanceTestPtr(jobbatch.ID("P")), Ref: acceptanceTestPtr("acceptance-process"), Spec: &Spec{Task: &TaskSpec{PayloadRef: "p"}}}, {Action: ActionAwaitState, Node: acceptanceTestPtr(jobbatch.ID("P")), State: acceptanceTestPtr(jobbatch.StateCompleted)}, {Action: ActionClose, Expect: acceptanceTestPtr(CloseOK)}}}
	got, data, records := acceptanceTestRun(t, p, false)
	if got.ExitCode != 0 {
		t.Fatalf("process run=%d", got.ExitCode)
	}
	if replay := Replay(p, bytes.NewReader(data)); replay.ExitCode != 0 || !reflect.DeepEqual(got.Footer, replay.Footer) {
		t.Fatal("process journal replay differs")
	}
	acceptanceTestPublicSweep(t, p, records, true)
}

func TestAcceptanceReplayRejectsAlteredRootFooter(t *testing.T) {
	// L4/L6/V-d mutation: bypassing replay root comparison accepts forged leaf counts in an otherwise intact journal.
	synctest.Test(t, func(t *testing.T) {
		s := acceptanceTestLoad(t, "f-d")
		got, data, _ := acceptanceTestRun(t, s, false)
		if got.ExitCode != ExitSuccess {
			t.Fatal("baseline journal failed")
		}
		lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
		var footer JournalFooter
		if err := json.Unmarshal(lines[len(lines)-1], &footer); err != nil {
			t.Fatal(err)
		}
		footer.Roots[0].Counts.Completed++
		encoded, err := json.Marshal(footer)
		if err != nil {
			t.Fatal(err)
		}
		lines[len(lines)-1] = encoded
		if replay := Replay(s, bytes.NewReader(bytes.Join(lines, []byte("\n")))); replay.ExitCode != ExitInvalid {
			t.Fatalf("forged root counts accepted: replay exit=%d want=2", replay.ExitCode)
		}
	})
}

func acceptanceTestAwaitRecord(t *testing.T, ctx context.Context, r *jobbatch.Runner, kind jobbatch.RecordType) {
	t.Helper()
	cursor := uint64(1)
	for {
		page, err := r.Events(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Records {
			if record.Type == kind {
				return
			}
		}
		cursor = page.Next
	}
}
