package batchcore

import (
	"context"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type attachFunc func(context.Context, AttachProbe) (jobbatch.ID, bool, error)

func (f attachFunc) ProbeLive(ctx context.Context, probe AttachProbe) (jobbatch.ID, bool, error) {
	return f(ctx, probe)
}

func TestIngressAttachedRootSettlesBeforeReturning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := AttachProbe{Owner: "owner", ToolCallID: "call", InputHash: "hash"}
	var order []string
	want := fantasy.ToolResponse{Type: "image", Data: []byte{1, 2}, MediaType: "image/png"}
	ports := FreshCall{
		Attach: attachFunc(func(_ context.Context, got AttachProbe) (jobbatch.ID, bool, error) {
			if got != probe {
				t.Fatalf("probe = %+v, want %+v", got, probe)
			}
			order = append(order, "attach")
			return "root", true, nil
		}),
		MatchLease: func(root jobbatch.ID) bool {
			order = append(order, "match lease")
			return root == "root"
		},
		ReleaseHandoff: func(context.Context) { order = append(order, "release handoff") },
		Deliver: func(deliveryCtx context.Context, root jobbatch.ID) (fantasy.ToolResponse, error) {
			if root != "root" {
				t.Fatalf("delivery root = %q", root)
			}
			if deliveryCtx == ctx {
				order = append(order, "deliver")
				cancel()
				return fantasy.ToolResponse{}, deliveryCtx.Err()
			}
			if deliveryCtx.Err() != nil {
				t.Fatalf("settlement context canceled: %v", deliveryCtx.Err())
			}
			order = append(order, "settle")
			return want, nil
		},
	}
	core := NewIngress()
	got, err := core.Invoke(ctx, probe, ports, nil)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Invoke = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	if wantOrder := []string{"attach", "match lease", "release handoff", "deliver", "settle"}; !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	if n := core.InFlightCalls(); n != 0 {
		t.Fatalf("in-flight calls after settling = %d", n)
	}
}
