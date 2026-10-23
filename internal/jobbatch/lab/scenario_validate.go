package lab

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func scenarioValidateAction(a Action) error {
	rules := map[ActionName]string{
		ActionSubmit: "ref root spec", ActionAdd: "ref parent items", ActionStop: "node",
		ActionInject: "node message_ref", ActionCapacityAvailable: "group", ActionRelease: "root",
		ActionClose: "?timeout_ms ?expect ?async", ActionAwaitStart: "node bind ?timeout_ms",
		ActionReleaseStart: "attempt decision ?state ?code ?timeout_ms", ActionComplete: "attempt state ?code ?timeout_ms",
		ActionEmit: "attempt text", ActionAwaitInject: "attempt message_ref ?timeout_ms",
		ActionHoldStop: "attempt", ActionFailStop: "attempt code", ActionAwaitStop: "attempt call ?timeout_ms",
		ActionReturnStop: "attempt call result", ActionHoldDispose: "attempt", ActionAwaitDispose: "attempt ?timeout_ms",
		ActionReturnDispose: "attempt result", ActionHoldRead: "attempt", ActionReturnRead: "attempt",
		ActionAwaitState: "node state ?timeout_ms", ActionExpectOutput: "node ?contains ?eof ?timeout_ms ?async",
		ActionJoin: "name ?timeout_ms", ActionAwaitRecord: "type ?node ?code ?timeout_ms",
	}
	rule, ok := rules[a.Action]
	if !ok {
		return scenarioInvalid("action")
	}
	allowed, required := map[string]bool{"action": true, "expect_error": true}, map[string]bool{}
	for _, f := range strings.Fields(rule) {
		name := strings.TrimPrefix(f, "?")
		allowed[name] = true
		if f == name {
			required[name] = true
		}
	}
	value, typ := reflect.ValueOf(a), reflect.TypeOf(a)
	for i := 1; i < value.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		present := !value.Field(i).IsNil()
		if present && !allowed[name] || !present && required[name] {
			return scenarioInvalid("action fields: " + name + " (rule: " + rule + ")")
		}
	}
	if a.Ref != nil && (*a.Ref == "" || !jobbatch.ValidRef(*a.Ref)) {
		return scenarioInvalid("ref")
	}
	if a.TimeoutMS != nil {
		if _, err := scenarioDuration(*a.TimeoutMS, true); err != nil {
			return err
		}
	}
	if a.ExpectError != nil && !scenarioErrorValid(*a.ExpectError) {
		return scenarioInvalid("expect_error")
	}
	if a.Expect != nil {
		if *a.Expect != CloseOK && *a.Expect != CloseTimeout && *a.Expect != CloseDispose {
			return scenarioInvalid("close expectation")
		}
		if a.ExpectError != nil {
			return scenarioInvalid("conflicting close expectations")
		}
	}
	for _, id := range []*jobbatch.ID{a.Root, a.Parent, a.Node} {
		if id != nil && *id == "" {
			return scenarioInvalid("node identity")
		}
	}
	if a.Action == ActionSubmit && strings.Contains(string(*a.Root), ".") {
		return scenarioInvalid("root identity")
	}
	if a.Call != nil && *a.Call == 0 {
		return scenarioInvalid("stop call")
	}
	if a.Result != nil && !scenarioGateValid(*a.Result) {
		return scenarioInvalid("gate result")
	}
	if a.Action == ActionExpectOutput && (a.Contains != nil && a.EOF != nil || a.Contains == nil && a.EOF == nil && a.ExpectError == nil) {
		return scenarioInvalid("output expectation")
	}
	if a.Action == ActionReleaseStart {
		d := StartDecision{Decision: *a.Decision}
		if a.State != nil {
			d.State = *a.State
		}
		if a.Code != nil {
			d.Code = *a.Code
		}
		if !controlledDecisionValid(d) || (d.Decision != DecisionAcceptComplete && a.State != nil) || ((d.Decision == DecisionAccept || d.Decision == DecisionRefuseCapacity) && a.Code != nil) {
			return scenarioInvalid("start decision")
		}
	}
	if a.State != nil {
		if a.Action == ActionAwaitState {
			if *a.State != "terminal" && !scenarioStateValid(*a.State) {
				return scenarioInvalid("state")
			}
		} else if !a.State.Reportable() {
			return scenarioInvalid("result state")
		}
	}
	if a.Type != nil && !scenarioRecordValid(*a.Type) {
		return scenarioInvalid("record type")
	}
	return nil
}

func scenarioErrorValid(e ErrorName) bool {
	switch e {
	case ErrorInvalidSpec, ErrorInvalidEvent, ErrorDuplicateRoot, ErrorUnknownNode, ErrorTerminal, ErrorNotTerminal, ErrorNotStarted, ErrorUnsupported, ErrorReleased, ErrorClosing, ErrorClosed, ErrorCloseTimeout, ErrorGap, ErrorExecutorPanic, ErrorExecutionDone, ErrorDispose, ErrorCapacity, ErrorUnknownAttempt, ErrorAttemptPhase:
		return true
	}
	return false
}

func scenarioStateValid(s jobbatch.State) bool {
	return s == jobbatch.StatePending || s.Active() || s.Terminal()
}

func scenarioRecordValid(t jobbatch.RecordType) bool {
	switch t {
	case jobbatch.RecordSubmit, jobbatch.RecordAdd, jobbatch.RecordStopRequested, jobbatch.RecordStarted, jobbatch.RecordStartRejected, jobbatch.RecordSettled, jobbatch.RecordControl, jobbatch.RecordCapacityAvailable, jobbatch.RecordDeadlineExpired, jobbatch.RecordHostClosing, jobbatch.RecordRelease, jobbatch.RecordCommand, jobbatch.RecordNotice, jobbatch.RecordClosed:
		return true
	}
	return false
}

func scenarioValidateExpect(e *Expect) error {
	if e == nil {
		return nil
	}
	if e.MaxActive != nil && *e.MaxActive < 0 || e.ExitCode != nil && (*e.ExitCode < 0 || *e.ExitCode > 5) {
		return scenarioInvalid("oracle number")
	}
	if e.Roots != nil {
		for _, r := range *e.Roots {
			if r.ID == "" || r.State != nil && !scenarioStateValid(*r.State) || !scenarioCountsValid(r.Counts) {
				return scenarioInvalid("root oracle")
			}
		}
	}
	if e.Nodes != nil {
		for _, n := range *e.Nodes {
			if n.ID == "" || n.State != nil && !scenarioStateValid(*n.State) || !scenarioCauseValid(n.Cause) {
				return scenarioInvalid("node oracle")
			}
			for _, p := range []*int{n.Attempts, n.Starts, n.Accepted, n.Stops, n.Disposes} {
				if p != nil && *p < 0 {
					return scenarioInvalid("oracle counter")
				}
			}
			if n.Outcome != nil {
				if n.Outcome.State != nil && *n.Outcome.State != "" && !n.Outcome.State.Reportable() || n.Outcome.Code != nil && !jobbatch.ValidCode(*n.Outcome.Code) {
					return scenarioInvalid("outcome oracle")
				}
			}
			if n.Control != nil {
				c := *n.Control
				if c != "none" && c != "pending" && c != "acked" && !(strings.HasPrefix(c, "failed(") && strings.HasSuffix(c, ")") && jobbatch.ValidCode(c[7:len(c)-1]) && c[7:len(c)-1] != "") {
					return scenarioInvalid("control oracle")
				}
			}
		}
	}
	if e.Notices != nil {
		for root, notices := range *e.Notices {
			if root == "" {
				return scenarioInvalid("notice root")
			}
			for _, n := range notices {
				if n.Node == "" || n.State != nil && !n.State.Terminal() || !scenarioCauseValid(n.Cause) || !scenarioCountsValid(n.Counts) {
					return scenarioInvalid("notice oracle")
				}
			}
		}
	}
	if e.Before != nil {
		for _, pair := range *e.Before {
			if pair[0] == "" || pair[1] == "" {
				return scenarioInvalid("before oracle")
			}
		}
	}
	return nil
}

func scenarioCauseValid(c *jobbatch.Cause) bool {
	return c == nil || *c == "" || *c == jobbatch.CauseStop || *c == jobbatch.CauseTimeout || *c == jobbatch.CauseHostClosing || *c == jobbatch.CauseStopOnFail
}

func scenarioCountsValid(c *jobbatch.Counts) bool {
	if c == nil {
		return true
	}
	b, _ := json.Marshal(c)
	var counts map[string]int
	_ = json.Unmarshal(b, &counts)
	for _, n := range counts {
		if n < 0 {
			return false
		}
	}
	return true
}
