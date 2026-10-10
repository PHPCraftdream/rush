package protocol_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/memory"
	"github.com/PHPCraftdream/rush/internal/tasktree/protocol"
)

func unchangedEnvelope(t *testing.T, b *board, before tasktree.Envelope) {
	t.Helper()
	if !reflect.DeepEqual(before, b.envelope()) {
		t.Fatal("request changed snapshot, counter, revision or receipts")
	}
}

func TestProtocolPayloadByteBoundary(t *testing.T) {
	b := newBoard(t)
	success(t, b.call(`{"op":"init","expected_revision":0,"items":["A"]}`), 1)
	before := b.envelope()
	raw := `{"op":"view"}`
	raw += strings.Repeat(" ", protocol.MaxPayloadBytes-len(raw))
	success(t, b.call(raw), 1)
	unchangedEnvelope(t, b, before)
	problem(t, b.call(raw+" "), tasktree.CodeInvalidInput, 1)
	unchangedEnvelope(t, b, before)
}

func TestProtocolExpectedRevisionIntegerBoundary(t *testing.T) {
	b := newBoard(t)
	success(t, b.call(`{"op":"init","expected_revision":0,"items":["A"]}`), 1)
	before := b.envelope()
	summary := b.call(`{"op":"view"}`).Details.Summary
	for _, revision := range []string{"1.0", "1e2", `"1"`, "18446744073709551616", "-0"} {
		t.Run(revision, func(t *testing.T) {
			result := b.call(`{"op":"edit","id":"n1","title":"B","expected_revision":` + revision + `}`)
			problem(t, result, tasktree.CodeInvalidInput, 1)
			if !reflect.DeepEqual(summary, result.Details.Summary) {
				t.Fatal("rejection omitted current summary")
			}
			unchangedEnvelope(t, b, before)
		})
	}
}

func nestedDraft(depth int) string {
	raw := `{"kind":"task","title":"Leaf"}`
	for i := 1; i < depth; i++ {
		raw = `{"kind":"group","title":"Group","children":[` + raw + `]}`
	}
	return raw
}

func TestProtocolDraftDomainDepthBoundary(t *testing.T) {
	b := newBoard(t)
	success(t, b.call(`{"op":"init","expected_revision":0,"list":[`+nestedDraft(protocolLimits.MaxDepth)+`]}`), 1)
	before := b.envelope()
	result := b.call(`{"op":"add","expected_revision":1,"list":[` + nestedDraft(protocolLimits.MaxDepth+1) + `]}`)
	problem(t, result, tasktree.CodeLimitExceeded, 1)
	unchangedEnvelope(t, b, before)
}

func TestProtocolPayloadStructuralDepthBoundary(t *testing.T) {
	limits := protocolLimits
	limits.MaxDepth = 64
	limits.MaxNodes = 150
	store, err := memory.NewStore(limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &board{t: t, store: store, api: protocol.New(tasktree.NewService(store, limits)), key: "depth", actor: tasktree.Actor{Kind: tasktree.ActorAgent, ID: "agent"}}
	// Wrapper + list + 63 group objects + 63 children arrays = 128.
	draft := `{"kind":"group","title":"Empty","children":[]}`
	for i := 1; i < 63; i++ {
		draft = `{"kind":"group","title":"G","children":[` + draft + `]}`
	}
	success(t, b.call(`{"op":"init","expected_revision":0,"list":[`+draft+`]}`), 1)
	before := b.envelope()
	// One task in the innermost array adds exactly one structural level.
	deeper := strings.Replace(draft, `"children":[]`, `"children":[{"kind":"task","title":"A"}]`, 1)
	problem(t, b.call(`{"op":"add","expected_revision":1,"list":[`+deeper+`]}`), tasktree.CodeInvalidInput, 1)
	unchangedEnvelope(t, b, before)
}

func TestProtocolPayloadQuotedContainers(t *testing.T) {
	b := newBoard(t)
	for i, title := range []string{
		strings.Repeat("{[", protocol.MaxPayloadDepth+1),
		`escaped quote " { [ and backslash \ } ]`,
		`trailing backslash \`,
	} {
		// Use a selector on a persisted label to keep this a valid read-only request.
		label, err := json.Marshal(title)
		if err != nil {
			t.Fatal(err)
		}
		limits := protocolLimits
		limits.MaxTitleBytes = len(title) + 1
		store, err := memory.NewStore(limits, nil)
		if err != nil {
			t.Fatal(err)
		}
		b.store, b.api = store, protocol.New(tasktree.NewService(store, limits))
		b.key = tasktree.TreeKey(fmt.Sprintf("quoted%d", i))
		success(t, b.call(`{"op":"init","expected_revision":0,"items":[`+string(label)+`]}`), 1)
		before := b.envelope()
		success(t, b.call(`{"op":"view","text":`+string(label)+`}`), 1)
		unchangedEnvelope(t, b, before)
	}
}
