package batchdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type testExecution struct{ jobbatch.Execution }

func TestExecutorLeaseHandoff(t *testing.T) {
	bindingErr := errors.New("binding unavailable")
	startErr := errors.New("start failed")
	for _, tc := range []struct {
		name         string
		execution    jobbatch.Execution
		err, wantErr error
		want         []string
	}{
		{"accepted", &testExecution{}, nil, nil, []string{"prepare", "start", "decorate", "wrap", "release"}},
		{"start error", nil, startErr, startErr, []string{"prepare", "start", "release"}},
		{"nil execution", nil, nil, bindingErr, []string{"prepare", "start", "release"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			var releaseLease func()
			e := Executor[struct{}, int]{
				Family: func(string) (LeafFamily[struct{}, int], bool) {
					return func(_ context.Context, _ struct{}, _ jobbatch.Launch, p int, report func(jobbatch.Result)) (jobbatch.Execution, error) {
						events = append(events, "start")
						if p != 42 || report == nil {
							t.Fatal("lost launch payload or reporter")
						}
						report(jobbatch.Result{Code: "forwarded"})
						return tc.execution, tc.err
					}, true
				},
				Prepare: func(context.Context, struct{}, json.RawMessage) (int, func(), error) {
					events = append(events, "prepare")
					return 42, func() { events = append(events, "release") }, nil
				},
				DecorateExecution: func(jobbatch.Execution) jobbatch.Execution {
					events = append(events, "decorate")
					return &testExecution{}
				},
				WithLease: func(execution jobbatch.Execution, release func()) jobbatch.Execution {
					events = append(events, "wrap")
					if execution == tc.execution {
						t.Fatal("lease wrapper got undecorated execution")
					}
					releaseLease = release
					return execution
				},
				StartError:         func(_ string, err error) error { return err },
				BindingUnavailable: bindingErr,
			}
			var received string
			execution, err := e.Start(context.Background(), jobbatch.Launch{}, func(r jobbatch.Result) { received = r.Code })
			if err != tc.wantErr || (execution != nil) != (err == nil) || received != "forwarded" {
				t.Fatalf("execution=%v err=%v report=%q", execution, err, received)
			}
			if releaseLease != nil {
				if !reflect.DeepEqual(events, tc.want[:len(tc.want)-1]) {
					t.Fatalf("before release: %v", events)
				}
				releaseLease()
			}
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events = %v; want %v", events, tc.want)
			}
		})
	}
}
