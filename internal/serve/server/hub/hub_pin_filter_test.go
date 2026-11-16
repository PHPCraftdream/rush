package hub

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/stretchr/testify/require"
)

func TestEventSessionClassificationExhaustive(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	// The protocol constants moved to the wire leaf package: the former
	// protocol.go is now wire/protocol.go plus the live-work section in
	// wire/snapshots.go.
	var protocolDecls []ast.Decl
	for _, name := range []string{"protocol.go", "snapshots.go"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(file), "..", "wire", name), nil, parser.ParseComments)
		require.NoError(t, err)
		protocolDecls = append(protocolDecls, parsed.Decls...)
	}
	values := map[string]string{}
	for _, decl := range protocolDecls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if !strings.HasPrefix(name.Name, "Event") {
					continue
				}
				exprIndex := i
				if exprIndex >= len(valueSpec.Values) {
					exprIndex = len(valueSpec.Values) - 1
				}
				require.GreaterOrEqual(t, exprIndex, 0, "event %s must have an explicit string value", name.Name)
				literal, ok := valueSpec.Values[exprIndex].(*ast.BasicLit)
				require.True(t, ok, "event %s must have a string literal value", name.Name)
				value, err := strconv.Unquote(literal.Value)
				require.NoError(t, err)
				values[value] = name.Name
			}
		}
	}
	for value, name := range values {
		class, ok := eventClasses[value]
		require.True(t, ok, "event constant %s=%q lacks explicit classification", name, value)
		require.NotEqual(t, eventUnknown, class, "event constant %s=%q has unknown classification", name, value)
	}
	for value := range eventClasses {
		require.Contains(t, values, value, "classification %q has no Event* constant", value)
	}
}

func isPinnedViewDeliveryMarker(message wire.WSMessage, sessionID string) bool {
	if message.Type != wire.EventSessionCreated {
		return false
	}
	var marker struct {
		SessionID string `json:"sessionID"`
		ID        string `json:"id"`
	}
	if json.Unmarshal(message.Payload, &marker) != nil {
		return false
	}
	return marker.SessionID == sessionID && marker.ID == "delivery-barrier"
}

func TestPinnedViewDeliveryMarkerMatchesExactMarker(t *testing.T) {
	valid := wire.WSMessage{Type: wire.EventSessionCreated, Payload: json.RawMessage(`{"sessionID":"own","id":"delivery-barrier"}`)}
	for _, tc := range []struct {
		name      string
		message   wire.WSMessage
		sessionID string
		want      bool
	}{
		{name: "exact marker", message: valid, sessionID: "own", want: true},
		{name: "foreign session", message: valid, sessionID: "foreign"},
		{name: "other marker id", message: wire.WSMessage{Type: wire.EventSessionCreated, Payload: json.RawMessage(`{"sessionID":"own","id":"other"}`)}, sessionID: "own"},
		{name: "unrelated session created", message: wire.WSMessage{Type: wire.EventSessionCreated, Payload: json.RawMessage(`{"sessionID":"own","id":"ordinary"}`)}, sessionID: "own"},
		{name: "malformed payload", message: wire.WSMessage{Type: wire.EventSessionCreated, Payload: json.RawMessage(`{`)}, sessionID: "own"},
		{name: "other event type", message: wire.WSMessage{Type: wire.EventSessionUpdated, Payload: valid.Payload}, sessionID: "own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isPinnedViewDeliveryMarker(tc.message, tc.sessionID))
		})
	}
}

func TestHubMarkerWaitsForPriorDeliveryBeforeBroadcast(t *testing.T) {
	h := NewHub()
	t.Cleanup(func() { close(h.stopped) })
	result := make(chan error, 1)
	go func() {
		result <- broadcastMarkerAfterDelivery(h, map[string]string{"sessionID": "own", "id": "delivery-barrier"})
	}()

	var ack chan struct{}
	ackClosed := false
	defer func() {
		if ack != nil && !ackClosed {
			close(ack)
		}
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case ack = <-h.deliveryBarrier:
	case err := <-result:
		t.Fatalf("marker helper returned before its barrier was accepted: %v", err)
	case <-timer.C:
		t.Fatal("marker helper did not request a delivery barrier")
	}
	select {
	case raw := <-h.broadcast:
		t.Fatalf("marker was queued before the barrier acknowledgement: type=%q", envelopeType(raw))
	default:
	}

	close(ack)
	ackClosed = true
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-timer.C:
		t.Fatal("marker helper did not finish after barrier acknowledgement")
	}
	select {
	case raw := <-h.broadcast:
		var message wire.WSMessage
		require.NoError(t, json.Unmarshal(raw, &message))
		require.True(t, isPinnedViewDeliveryMarker(message, "own"), "queued event is not the exact delivery marker")
	case <-timer.C:
		t.Fatal("marker helper did not queue the marker after barrier acknowledgement")
	}
}

func TestClientReplyAppliesPinnedViewExceptCorrelatedErrors(t *testing.T) {
	client := &Client{send: make(chan []byte, 8), auth: NewClientAuth(apiauth.Principal{PinnedSession: "own", Scopes: []string{"read"}}, CommandPolicy{})}
	drain := func() []string {
		var out []string
		for {
			select {
			case msg := <-client.send:
				out = append(out, string(msg))
			default:
				return out
			}
		}
	}

	client.Reply("a", wire.EventSessionUpdated, map[string]string{"sessionID": "foreign"}, "")
	client.Reply("b", wire.EventSessionsList, []map[string]string{{"id": "own"}, {"id": "foreign"}}, "")
	client.Reply("c", wire.EventLogs, map[string]string{"line": "SECRET"}, "")
	require.Empty(t, drain(), "replies about foreign sessions or non-session classes must not reach a pinned client")

	client.Reply("d", wire.EventSessionUpdated, map[string]string{"sessionID": "own"}, "")
	require.Len(t, drain(), 1, "a reply about the pinned session is delivered")

	client.Reply("e", wire.EventError, nil, "not found")
	client.Reply("f", wire.EventResponse, map[string]string{"ok": "true"}, "")
	require.Len(t, drain(), 2, "correlated error and response replies bypass the view")

	client.Reply("", wire.EventError, nil, "uncorrelated")
	require.Empty(t, drain(), "an error without a request id is an event and stays filtered")
}

func TestPinnedViewNeverAllowsInvalidMetadata(t *testing.T) {
	view := newPinnedView("own")
	for _, meta := range []eventMetadata{
		{valid: false, class: eventSafeGlobal, msgType: wire.EventUpdateAvailable},
		{valid: false, class: eventSession, sessionIDs: []string{"own"}},
	} {
		require.False(t, view.allows(meta), "%+v", meta)
	}
	require.True(t, view.allows(eventMetadata{valid: true, class: eventSession, sessionIDs: []string{"own"}}))
	require.True(t, newPinnedView("").allows(eventMetadata{}), "an unpinned view allows everything")
}

// WaitDelivery is the barrier tests read through; a sticky fan-out still
// queued behind it would arrive after the frame the test stops at. Many
// clients keep that late fan-out busy long enough for the check to see it.
func TestHubDeliveryBarrierIncludesPendingStickyFanOut(t *testing.T) {
	for i := 0; i < 20; i++ {
		h := NewHub()
		clients := make([]*Client, 3000)
		for j := range clients {
			clients[j] = &Client{hub: h, send: make(chan []byte, 1)}
			h.clients[clients[j]] = struct{}{}
		}
		h.BroadcastSticky(wire.EventSkills, map[string]string{"skill": "S"})
		barrier := make(chan (<-chan struct{}), 1)
		go func() { barrier <- h.WaitDelivery() }() // parks until Run starts, so both inputs are ready at once
		ctx, cancel := context.WithCancel(t.Context())
		go h.Run(ctx)
		select {
		case done := <-barrier:
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("barrier did not complete")
			}
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("barrier was not accepted")
		}
		for j, client := range clients {
			select {
			case msg := <-client.send:
				require.Contains(t, string(msg), `"skill":"S"`)
			default:
				cancel()
				t.Fatalf("iteration %d: client %d had no sticky event when the barrier completed", i, j)
			}
		}
		cancel()
	}
}

func broadcastMarkerAfterDelivery(h *Hub, payload any) error {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	done := make(chan struct{})
	select {
	case h.deliveryBarrier <- done:
	case <-h.stopped:
		return errors.New("hub stopped before delivery barrier was accepted")
	case <-timer.C:
		return errors.New("timed out submitting hub delivery barrier")
	}
	select {
	case <-h.stopped:
		return errors.New("hub stopped before delivery barrier completed")
	default:
	}
	select {
	case <-done:
	case <-h.stopped:
		return errors.New("hub stopped before delivery barrier completed")
	case <-timer.C:
		return errors.New("timed out waiting for hub delivery barrier")
	}
	select {
	case <-h.stopped:
		return errors.New("hub stopped before marker broadcast")
	default:
	}
	h.Broadcast(wire.EventSessionCreated, payload)
	return nil
}
