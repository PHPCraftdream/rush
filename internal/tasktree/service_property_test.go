package tasktree_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	tt "github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/lab"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
)

const servicePropertySeed int64 = 130503

type propertyInvocation struct {
	actor    tt.Actor
	id       tt.RequestID
	revision tt.Revision
	command  tt.Command
}

type serviceProperty struct {
	t       *testing.T
	limits  tt.Limits
	store   *memory.Store
	service *tt.Service
	model   tt.Envelope
	history []propertyInvocation
	counts  map[string]int
	steps   int
}

func newServiceProperty(t *testing.T) *serviceProperty {
	t.Helper()
	t.Logf("seed=%d", servicePropertySeed)
	limits := tt.Limits{MaxNodes: 256, MaxDepth: 8, MaxTitleBytes: 100, MaxReasonBytes: 100, MaxTombstones: 256, MaxReceipts: 256}
	store, err := memory.NewStore(limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := store.Load(context.Background(), "board")
	if err != nil {
		t.Fatal(err)
	}
	return &serviceProperty{t: t, limits: limits, store: store, service: tt.NewService(store, limits), model: envelope, counts: make(map[string]int)}
}

func propertyJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func propertyEqual(t *testing.T, label string, want, got any) {
	t.Helper()
	if a, b := propertyJSON(t, want), propertyJSON(t, got); !bytes.Equal(a, b) {
		t.Fatalf("seed=%d %s: want %s, got %s", servicePropertySeed, label, a, b)
	}
}

func propertyTree(t *testing.T, e tt.Envelope, limits tt.Limits) *tt.Tree {
	t.Helper()
	tree, err := tt.Restore(e.Snapshot, limits)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func propertyReceipt(t *testing.T, in propertyInvocation, delta tt.Delta) tt.Receipt {
	t.Helper()
	payload := propertyJSON(t, struct {
		Revision tt.Revision `json:"expected_revision"`
		Command  tt.Command  `json:"command"`
	}{in.revision, in.command})
	digest := sha256.Sum256(payload)
	return tt.Receipt{RequestID: in.id, Actor: in.actor, Fingerprint: hex.EncodeToString(digest[:]), CommittedRevision: in.revision + 1, Delta: delta}
}

func (p *serviceProperty) load() tt.Envelope {
	p.t.Helper()
	e, err := p.store.Load(context.Background(), "board")
	if err != nil {
		p.t.Fatal(err)
	}
	return e
}

func (p *serviceProperty) checkpoint() {
	p.t.Helper()
	checkpoint := lab.Checkpoint{SchemaVersion: lab.SchemaVersion, TreeKey: "board", Limits: p.limits, Envelope: p.load()}
	var buf bytes.Buffer
	if err := lab.WriteCheckpoint(&buf, checkpoint); err != nil {
		p.t.Fatal(err)
	}
	loaded, err := lab.ReadCheckpoint(&buf)
	if err != nil {
		p.t.Fatal(err)
	}
	report, err := lab.Run(context.Background(), lab.Scenario{SchemaVersion: lab.SchemaVersion, TreeKey: "board", Limits: p.limits}, &loaded)
	if err != nil {
		p.t.Fatal(err)
	}
	propertyEqual(p.t, "checkpoint resume", p.model, report.Checkpoint.Envelope)
	store, err := memory.NewStore(p.limits, map[tt.TreeKey]tt.Envelope{"board": report.Checkpoint.Envelope})
	if err != nil {
		p.t.Fatal(err)
	}
	p.store, p.service = store, tt.NewService(store, p.limits)
	p.counts["checkpoint"]++
}

func (p *serviceProperty) reply(receipt tt.Receipt, replay bool) tt.MutationReply {
	tree := propertyTree(p.t, p.model, p.limits)
	summary := tree.Summary()
	summary.TreeKey, summary.Revision = "board", p.model.Revision
	return tt.MutationReply{Receipt: receipt, Replayed: replay, Summary: summary, Created: tree.Briefs(receipt.Delta.Created)}
}

func (p *serviceProperty) step(in propertyInvocation, code tt.ProblemCode, branch string) {
	p.t.Helper()
	p.steps++
	before := p.load()
	propertyEqual(p.t, "before model", p.model, before)
	tree := propertyTree(p.t, p.model, p.limits)
	old, replay := p.model.Receipts[in.id]
	var delta tt.Delta
	if code == "" && !replay {
		var err error
		delta, err = tree.Apply(in.actor, in.command)
		if err != nil {
			var problem *tt.Problem
			if !errors.As(err, &problem) {
				p.t.Fatal(err)
			}
			code = problem.Code
		}
	}
	got, err := p.service.Mutate(context.Background(), "board", in.actor, in.id, in.revision, in.command)
	if code != "" {
		serviceTestCode(p.t, err, code)
		if code == tt.CodeConflict {
			var problem *tt.Problem
			errors.As(err, &problem)
			if problem.Expected != in.revision || problem.Current != before.Revision {
				p.t.Fatalf("incorrect versioned conflict: %+v", problem)
			}
		}
		propertyEqual(p.t, "error no write", before, p.load())
		p.counts["error:"+string(code)]++
	} else {
		if err != nil {
			p.t.Fatalf("seed=%d step=%d branch=%s: %v", servicePropertySeed, p.steps, branch, err)
		}
		if replay {
			propertyEqual(p.t, "historical receipt/current reply", p.reply(old, true), got)
			propertyEqual(p.t, "replay no write", before, p.load())
			again, err := p.service.Mutate(context.Background(), "board", in.actor, in.id, in.revision, in.command)
			if err != nil {
				p.t.Fatal(err)
			}
			propertyEqual(p.t, "same state replay reply", got, again)
			propertyEqual(p.t, "second replay no write", before, p.load())
			p.counts["replay"]++
		} else {
			p.model.Snapshot = tree.Snapshot()
			p.model.Revision++
			receipt := propertyReceipt(p.t, in, delta)
			p.model.Receipts[in.id] = receipt
			propertyEqual(p.t, "fresh reply", p.reply(receipt, false), got)
			after := p.load()
			if after.Revision != before.Revision+1 || len(after.Receipts) != len(before.Receipts)+1 {
				p.t.Fatal("commit did not advance version and receipt count exactly once")
			}
			p.history = append(p.history, in)
			p.counts["success:"+string(in.command.Op)]++
		}
	}
	propertyEqual(p.t, "after model", p.model, p.load())
	p.counts[branch]++
	p.checkpoint()
}

func (p *serviceProperty) fresh(command tt.Command, actor tt.Actor, branch string) {
	p.step(propertyInvocation{actor: actor, id: tt.RequestID(fmt.Sprintf("request-%d", p.steps)), revision: p.model.Revision, command: command}, "", branch)
}

func TestServiceSeededCommandProperties(t *testing.T) {
	p := newServiceProperty(t)
	rng := rand.New(rand.NewSource(servicePropertySeed))
	p.fresh(serviceTestInit(), serviceTestActor, "init")
	for _, op := range []tt.Operation{tt.OpEdit, tt.OpStart, tt.OpBlock, tt.OpUnblock, tt.OpDone, tt.OpReopen, tt.OpDrop, tt.OpReopen, tt.OpRemove} {
		title := fmt.Sprintf("edited-%d", rng.Intn(10000))
		c := tt.Command{Op: op, Target: tt.Selector{ID: "n1"}}
		actor := serviceTestActor
		if op == tt.OpEdit {
			c.Title = &title
		}
		if op == tt.OpBlock || op == tt.OpDrop {
			c.Reason = "property reason"
		}
		if op == tt.OpReopen || op == tt.OpRemove {
			actor = serviceTestOperator
		}
		p.fresh(c, actor, "lifecycle")
	}
	p.step(p.history[0], "", "old-replay-after-remove")
	branches := []string{"add", "edit", "start", "done", "block", "unblock", "drop", "reopen", "remove", "invalid-selector", "missing-selector", "unknown-selector", "invalid-title", "stale", "future", "reuse-actor", "reuse-payload", "reuse-revision", "old-replay", "forbidden"}
	for cycle := 0; cycle < 6; cycle++ {
		for _, index := range rng.Perm(len(branches)) {
			branch := branches[index]
			ids := make([]tt.NodeID, 0)
			for id, node := range p.model.Snapshot.Nodes {
				if node.Kind == tt.KindTask {
					ids = append(ids, id)
				}
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			if len(ids) == 0 {
				p.fresh(tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: fmt.Sprintf("refill-%d", p.steps)}}}, serviceTestActor, "refill")
				ids = []tt.NodeID{tt.NodeID(fmt.Sprintf("n%d", p.model.Snapshot.NextID-1))}
			}
			id := ids[rng.Intn(len(ids))]
			title := fmt.Sprintf("title-%d-%d", p.steps, rng.Intn(10000))
			in := propertyInvocation{actor: serviceTestActor, id: tt.RequestID(fmt.Sprintf("request-%d", p.steps)), revision: p.model.Revision, command: tt.Command{Op: tt.Operation(branch), Target: tt.Selector{ID: id}}}
			var code tt.ProblemCode
			switch branch {
			case "add":
				in.command = tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: title}}}
			case "edit":
				in.command.Title = &title
			case "block", "drop":
				in.command.Reason = "random reason"
			case "reopen":
				in.actor = serviceTestOperator
			case "remove":
				in.actor, in.command.Op = serviceTestOperator, tt.OpRemove
			case "invalid-selector":
				in.command = tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: id, Text: "both"}}
			case "missing-selector":
				in.command = tt.Command{Op: tt.OpEdit, Title: &title}
			case "unknown-selector":
				in.command = tt.Command{Op: tt.OpDone, Target: tt.Selector{ID: "n999999"}}
			case "invalid-title":
				title = " "
				in.command = tt.Command{Op: tt.OpEdit, Target: tt.Selector{ID: id}, Title: &title}
			case "stale", "future":
				in.command = tt.Command{Op: tt.OpEdit, Target: tt.Selector{ID: id}, Title: &title}
				if branch == "stale" {
					in.revision--
				} else {
					in.revision += tt.Revision(1 + rng.Intn(4))
				}
				code = tt.CodeConflict
			case "reuse-actor", "reuse-payload", "reuse-revision", "old-replay":
				in = p.history[rng.Intn(len(p.history))]
				switch branch {
				case "reuse-actor":
					in.actor.ID += "-other"
					code = tt.CodeRequestReused
				case "reuse-payload":
					in.command.Reason += "changed"
					code = tt.CodeRequestReused
				case "reuse-revision":
					in.revision++
					code = tt.CodeRequestReused
				}
			case "forbidden":
				in.command.Op = tt.OpRemove
				code = tt.CodeForbidden
			}
			p.step(in, code, branch)
		}
	}
	for _, branch := range branches {
		if p.counts[branch] == 0 {
			t.Fatalf("seed=%d unexercised branch %s", servicePropertySeed, branch)
		}
	}
	for _, op := range []tt.Operation{tt.OpInit, tt.OpAdd, tt.OpEdit, tt.OpStart, tt.OpDone, tt.OpBlock, tt.OpUnblock, tt.OpDrop, tt.OpReopen, tt.OpRemove} {
		if p.counts["success:"+string(op)] == 0 {
			t.Fatalf("seed=%d no successful %s", servicePropertySeed, op)
		}
	}
	t.Logf("seed=%d steps=%d branch counts=%v", servicePropertySeed, p.steps, p.counts)
}

func TestServiceSeededConcurrentCASProperties(t *testing.T) {
	p := newServiceProperty(t)
	rng := rand.New(rand.NewSource(servicePropertySeed + 1))
	t.Logf("concurrent seed=%d", servicePropertySeed+1)
	p.fresh(serviceTestInit(), serviceTestActor, "init")
	type result struct {
		in    propertyInvocation
		reply tt.MutationReply
		err   error
	}
	for round := 0; round < 12; round++ {
		before := p.load()
		n := 3 + rng.Intn(4)
		start := make(chan struct{})
		ready := make(chan struct{}, n)
		results := make(chan result, n)
		for worker := 0; worker < n; worker++ {
			in := propertyInvocation{actor: serviceTestActor, id: tt.RequestID(fmt.Sprintf("round-%d-worker-%d", round, worker)), revision: before.Revision, command: tt.Command{Op: tt.OpAdd, Drafts: []tt.Draft{{Kind: tt.KindTask, Title: fmt.Sprintf("candidate-%d-%d-%d", round, worker, rng.Intn(10000))}}}}
			go func(in propertyInvocation) {
				ready <- struct{}{}
				<-start
				reply, err := p.service.Mutate(context.Background(), "board", in.actor, in.id, in.revision, in.command)
				results <- result{in: in, reply: reply, err: err}
			}(in)
		}
		for worker := 0; worker < n; worker++ {
			<-ready
		}
		close(start)
		var winners []result
		var losers []result
		for worker := 0; worker < n; worker++ {
			r := <-results
			if r.err == nil {
				winners = append(winners, r)
			} else {
				losers = append(losers, r)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("seed=%d round=%d winners=%d", servicePropertySeed+1, round, len(winners))
		}
		for _, loser := range losers {
			serviceTestCode(t, loser.err, tt.CodeConflict)
			var problem *tt.Problem
			errors.As(loser.err, &problem)
			if problem.Expected != before.Revision || problem.Current != before.Revision+1 {
				t.Fatalf("incorrect CAS conflict: %+v", problem)
			}
		}
		winner := winners[0]
		tree := propertyTree(t, before, p.limits)
		delta, err := tree.Apply(winner.in.actor, winner.in.command)
		if err != nil {
			t.Fatal(err)
		}
		p.model.Snapshot, p.model.Revision = tree.Snapshot(), before.Revision+1
		receipt := propertyReceipt(t, winner.in, delta)
		p.model.Receipts[winner.in.id] = receipt
		propertyEqual(t, "winner reply", p.reply(receipt, false), winner.reply)
		after := p.load()
		if after.Revision != before.Revision+1 || len(after.Receipts) != len(before.Receipts)+1 {
			t.Fatal("CAS round must commit exactly once")
		}
		propertyEqual(t, "only winner durable", p.model, after)
		p.history = append(p.history, winner.in)
		p.counts["winner"]++
		p.counts["conflict"] += len(losers)
		p.checkpoint()
		for _, loser := range losers {
			p.step(loser.in, tt.CodeConflict, "isolated-rejection")
		}
		p.step(p.history[rng.Intn(len(p.history))], "", "retained-replay")
	}
	t.Logf("seed=%d branch counts=%v", servicePropertySeed+1, p.counts)
}
