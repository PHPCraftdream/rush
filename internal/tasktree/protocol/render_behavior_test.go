package protocol_test

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

func assertInitRevisionData(t *testing.T, result protocol.ToolResult) {
	t.Helper()
	matches := regexp.MustCompile(`expected_revision=([0-9]+)`).FindAllStringSubmatch(result.Text, -1)
	if len(matches) == 0 {
		t.Fatal("missing init revision data")
	}
	for _, match := range matches {
		revision, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil || tasktree.Revision(revision) != result.Details.Summary.Revision {
			t.Fatalf("init revision data differs from current summary: %s", match[0])
		}
	}
}

func TestProtocolSeededUninitializedRevisionAndLegacyGuards(t *testing.T) {
	for _, operatorInit := range []bool{false, true} {
		name := "agent_init_retains_guard"
		if operatorInit {
			name = "operator_init_explicitly_clears_guard"
		}
		t.Run(name, func(t *testing.T) {
			snapshot := tasktree.EmptySnapshot()
			guards := []tasktree.TitleGuard{{Kind: tasktree.KindTask, Title: "Legacy deleted"}}
			snapshot.TitleGuards = guards
			seed := tasktree.Envelope{Revision: 7, Snapshot: snapshot, Receipts: map[tasktree.RequestID]tasktree.Receipt{}}
			store, err := memory.NewStore(protocolLimits, map[tasktree.TreeKey]tasktree.Envelope{"imported": seed})
			if err != nil {
				t.Fatal(err)
			}
			b := &board{t: t, store: store, api: protocol.New(tasktree.NewService(store, protocolLimits)), key: "imported", actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent"}}
			before := b.envelope()
			if before.Revision != 7 || before.Snapshot.Initialized || len(before.Receipts) != 0 || !reflect.DeepEqual(before.Snapshot.TitleGuards, guards) {
				t.Fatal("seed did not retain imported state without fabricated receipts")
			}
			view := b.call(`{"op":"view"}`)
			success(t, view, 7)
			if view.Details.Summary.Initialized || view.Details.Summary.TreeKey != b.key || view.Details.View == nil || view.Details.View.Summary.Revision != 7 || view.Details.View.Root.ID != "n0" || len(view.Details.View.Root.Children) != 0 {
				t.Fatalf("imported view is not current: %+v", view.Details)
			}
			assertInitRevisionData(t, view)
			uninitialized := b.call(`{"op":"add","expected_revision":7,"items":["Allowed"]}`)
			problem(t, uninitialized, tasktree.CodeUninitialized, 7)
			assertInitRevisionData(t, uninitialized)
			problem(t, b.call(`{"op":"init","expected_revision":7,"items":["Allowed","Legacy deleted"]}`), tasktree.CodeRemovedByOperator, 7)
			if !reflect.DeepEqual(before, b.envelope()) {
				t.Fatal("view or rejected initialization changed imported state/guards/counter/receipts")
			}
			title := "Allowed"
			if operatorInit {
				b.actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}
				title = "Legacy deleted"
			}
			initialized := b.call(`{"op":"init","expected_revision":7,"items":[` + strconv.Quote(title) + `]}`)
			success(t, initialized, 8)
			e := b.envelope()
			if !initialized.Details.Summary.Initialized || initialized.Details.Receipt == nil || initialized.Details.Receipt.CommittedRevision != 8 || e.Revision != 8 || !e.Snapshot.Initialized || e.Snapshot.Nodes["n1"].Title != title || len(e.Receipts) != 1 {
				t.Fatal("valid imported initialization lost state or invented old receipt history")
			}
			if operatorInit {
				if len(e.Snapshot.TitleGuards) != 0 {
					t.Fatal("explicit operator init did not clear used guard")
				}
				return
			}
			if !reflect.DeepEqual(e.Snapshot.TitleGuards, guards) {
				t.Fatal("agent init discarded legacy guard")
			}
			problem(t, b.call(`{"op":"add","expected_revision":8,"items":["Legacy deleted"]}`), tasktree.CodeRemovedByOperator, 8)
			if !reflect.DeepEqual(e, b.envelope()) {
				t.Fatal("guarded add changed initialized board")
			}
			b.actor = tasktree.Actor{Kind: tasktree.ActorOperator, ID: "operator"}
			success(t, b.call(`{"op":"add","expected_revision":8,"items":["Legacy deleted"]}`), 9)
			e = b.envelope()
			if len(e.Snapshot.TitleGuards) != 0 || e.Snapshot.Nodes["n2"].Title != "Legacy deleted" || len(e.Receipts) != 2 {
				t.Fatal("explicit operator add did not clear guard and create new identity")
			}
		})
	}
}

func TestProtocolUnicodeReadableAndControlsEscaped(t *testing.T) {
	b := newBoard(t)
	title := "任务 café\n- forged\t\x1b"
	reason := "等待 décision\n# forged\r\x00"
	titleJSON, err := json.Marshal(title)
	if err != nil {
		t.Fatal(err)
	}
	created := b.call(`{"op":"init","expected_revision":0,"items":[` + string(titleJSON) + `]}`)
	success(t, created, 1)
	if len(created.Details.Created) != 1 || created.Details.Created[0].Title != title || !strings.Contains(created.Text, "任务 café") {
		t.Fatal("created title lost Unicode readability")
	}
	reasonJSON, err := json.Marshal(reason)
	if err != nil {
		t.Fatal(err)
	}
	success(t, b.call(`{"op":"block","expected_revision":1,"id":"n1","reason":`+string(reasonJSON)+`}`), 2)
	view := b.call(`{"op":"view"}`)
	success(t, view, 2)
	if view.Details.View == nil || len(view.Details.View.Root.Children) != 1 {
		t.Fatal("missing task view")
	}
	node := view.Details.View.Root.Children[0]
	if node.Title != title || node.Reason != reason || node.Status != tasktree.Blocked {
		t.Fatal("structured Unicode/control data changed")
	}
	for _, data := range []string{"任务 café", "等待 décision"} {
		if !strings.Contains(view.Text, data) {
			t.Fatalf("missing readable Unicode data %s", data)
		}
	}
	for _, control := range []string{"\n- forged", "\n# forged", "\t", "\r", "\x00", "\x1b"} {
		if strings.Contains(view.Text, control) {
			t.Fatal("unescaped user control data in text")
		}
	}
	e, err := b.store.Load(context.Background(), b.key)
	if err != nil {
		t.Fatal(err)
	}
	if e.Snapshot.Nodes["n1"].Title != title || e.Snapshot.Nodes["n1"].Reason != reason {
		t.Fatal("rendering altered persisted data")
	}
}
