package jobbatch

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestContractStatePredicates(t *testing.T) {
	cases := []struct {
		s                        State
		terminal, active, report bool
	}{
		{StatePending, false, false, false},
		{StateStarting, false, true, false},
		{StateRunning, false, true, false},
		{StateCancelling, false, true, false},
		{StateCompleted, true, false, true},
		{StateFailed, true, false, true},
		{StateCancelled, true, false, true},
		{StateTimedOut, true, false, true},
		{StateInterrupted, true, false, true},
		{StateSkipped, true, false, false},
		{State(""), false, false, false},
		{State("bogus"), false, false, false},
	}
	for _, c := range cases {
		if got := c.s.Terminal(); got != c.terminal {
			t.Errorf("%q.Terminal() = %v, want %v", c.s, got, c.terminal)
		}
		if got := c.s.Active(); got != c.active {
			t.Errorf("%q.Active() = %v, want %v", c.s, got, c.active)
		}
		if got := c.s.Reportable(); got != c.report {
			t.Errorf("%q.Reportable() = %v, want %v", c.s, got, c.report)
		}
	}
}

func TestContractCodeAndRefGrammar(t *testing.T) {
	validCodes := []string{"", "exit_3", "a.b-c_d", strings.Repeat("x", MaxCodeBytes)}
	for _, c := range validCodes {
		if !ValidCode(c) {
			t.Errorf("ValidCode(%q) = false", c)
		}
	}
	invalidCodes := []string{"Exit", "a b", "é", "a/b", "a:b", strings.Repeat("x", MaxCodeBytes+1)}
	for _, c := range invalidCodes {
		if ValidCode(c) {
			t.Errorf("ValidCode(%q) = true", c)
		}
	}
	validRefs := []string{"", "r-1", "A.b:c_d", strings.Repeat("R", MaxRefBytes)}
	for _, r := range validRefs {
		if !ValidRef(r) {
			t.Errorf("ValidRef(%q) = false", r)
		}
	}
	invalidRefs := []string{"a b", "a/b", "é", strings.Repeat("R", MaxRefBytes+1)}
	for _, r := range invalidRefs {
		if ValidRef(r) {
			t.Errorf("ValidRef(%q) = true", r)
		}
	}
}

func TestContractReservedCodes(t *testing.T) {
	reserved := []string{
		CodeStartFailed, CodeStartNilExecution, CodeStartPanic,
		CodeExecutorInvalidState, CodeInvalid, CodeStopFailed, CodeStopPanic,
		CodeStopTimeout, CodeDisposeFailed, CodeDisposePanic,
		CodeDisposeTimeout, CodeDisposePending,
	}
	if len(reserved) != 12 {
		t.Fatalf("reserved set has %d entries, want 12", len(reserved))
	}
	for _, c := range reserved {
		if !ReservedCode(c) || !ValidCode(c) {
			t.Errorf("code %q: reserved=%v valid=%v", c, ReservedCode(c), ValidCode(c))
		}
	}
	for _, c := range []string{"", "exit_1", "missing_program"} {
		if ReservedCode(c) {
			t.Errorf("ReservedCode(%q) = true", c)
		}
	}
}

func TestContractErrorCarriers(t *testing.T) {
	d := error(&DisposeError{Failures: []DisposeFailure{{Node: "r.1", Token: 3, Code: CodeDisposeTimeout}}})
	if !errors.Is(d, ErrDispose) {
		t.Error("DisposeError does not match ErrDispose")
	}
	if want := "r.1#3 dispose_timeout"; !strings.Contains(d.Error(), want) {
		t.Errorf("DisposeError text %q lacks %q", d.Error(), want)
	}
	c := error(&CloseTimeoutError{Nodes: []ID{"r.1", "r.2"}})
	if !errors.Is(c, ErrCloseTimeout) {
		t.Error("CloseTimeoutError does not match ErrCloseTimeout")
	}
	if want := "r.1, r.2"; !strings.Contains(c.Error(), want) {
		t.Errorf("CloseTimeoutError text %q lacks %q", c.Error(), want)
	}
	if errors.Is(d, ErrCloseTimeout) || errors.Is(c, ErrDispose) {
		t.Error("carriers match each other's sentinel")
	}
}

func TestContractJSONTagsAreSnakeCase(t *testing.T) {
	b, err := json.Marshal(NodeView{ID: "r", Root: "r", Kind: KindBatch, State: StateTimedOut})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"id"`, `"root"`, `"ordinal"`, `"kind"`, `"outcome"`, `"state":"timed_out"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("NodeView JSON %s lacks %s", b, key)
		}
	}
	lb, err := json.Marshal(Limits{MaxConcurrent: 2, ByGroup: map[string]int{"a": 1}, MaxDepth: 3, MaxNodes: 4})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"max_concurrent":2`, `"by_group":{"a":1}`, `"max_depth":3`, `"max_nodes":4`} {
		if !strings.Contains(string(lb), key) {
			t.Errorf("Limits JSON %s lacks %s", lb, key)
		}
	}
}
