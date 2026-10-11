package lab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func LoadScenario(r io.Reader) (*Scenario, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var s Scenario
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&s); err != nil {
		return nil, scenarioInvalid("scenario decoding")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, scenarioInvalid("trailing document")
	}
	if err := scenarioCheckJSON(data, reflect.TypeFor[Scenario]()); err != nil {
		return nil, err
	}
	if err := ValidateScenario(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func scenarioInvalid(reason string) error {
	return &scenarioRuleError{reason: reason}
}

type scenarioRuleError struct{ reason string }

func (e *scenarioRuleError) Error() string { return "invalid spec: " + e.reason }
func (e *scenarioRuleError) Unwrap() error { return jobbatch.ErrInvalidSpec }

// ScenarioValidationRule exposes only loader-owned rule text, never input data
// or an underlying I/O/JSON error. Action indexes are preserved by validation.
func ScenarioValidationRule(err error) string {
	var rule *scenarioRuleError
	if errors.As(err, &rule) {
		return rule.reason
	}
	return "scenario read"
}

func scenarioDuration(ms int64, positive bool) (time.Duration, error) {
	if ms < 0 || (positive && ms == 0) || ms > math.MaxInt64/int64(time.Millisecond) {
		return 0, scenarioInvalid("duration")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func ValidateScenario(s *Scenario) error {
	if s == nil || s.Version != 1 {
		return scenarioInvalid("version")
	}
	if s.Limits.MaxConcurrent <= 0 || s.Limits.MaxDepth <= 0 || s.Limits.MaxNodes <= 0 || s.Runner.RecordRetention <= 0 {
		return scenarioInvalid("limits")
	}
	for _, n := range s.Limits.ByGroup {
		if n <= 0 {
			return scenarioInvalid("group limit")
		}
	}
	if _, err := scenarioDuration(s.WaitBoundMS, true); err != nil {
		return err
	}
	if _, err := scenarioDuration(s.Runner.ControlTimeoutMS, true); err != nil {
		return err
	}
	for name, p := range s.Payloads {
		if name == "" {
			return scenarioInvalid("payload name")
		}
		switch p.Kind {
		case "controlled":
			if p.Program != nil || p.Args != nil || p.Cwd != nil || p.MaxOutputBytes != nil {
				return scenarioInvalid("controlled payload fields")
			}
		case "process":
			if p.Program == nil || *p.Program == "" || (p.MaxOutputBytes != nil && *p.MaxOutputBytes <= 0) {
				return scenarioInvalid("process payload")
			}
		default:
			return scenarioInvalid("payload kind")
		}
	}
	for name, msg := range s.Messages {
		if name == "" || !json.Valid(msg) {
			return scenarioInvalid("message")
		}
	}
	refs, binds, async, joined := map[string]bool{}, map[string]bool{}, map[string]ActionName{}, map[string]bool{}
	for i, a := range s.Script {
		if err := scenarioValidateAction(a); err != nil {
			return fmt.Errorf("action %d: %w", i+1, err)
		}
		if a.Ref != nil {
			if *a.Ref == "" || !jobbatch.ValidRef(*a.Ref) || refs[*a.Ref] {
				return scenarioInvalid("ref")
			}
			refs[*a.Ref] = true
		}
		if a.Bind != nil {
			if *a.Bind == "" || binds[*a.Bind] {
				return scenarioInvalid("binding")
			}
			binds[*a.Bind] = true
		}
		if a.Attempt != nil && !binds[*a.Attempt] {
			return scenarioInvalid("unbound attempt")
		}
		if a.MessageRef != nil {
			if _, ok := s.Messages[*a.MessageRef]; !ok {
				return scenarioInvalid("message reference")
			}
		}
		if a.Async != nil {
			if *a.Async == "" || async[*a.Async] != "" {
				return scenarioInvalid("async name")
			}
			async[*a.Async] = a.Action
		}
		if a.Action == ActionJoin {
			if async[*a.Name] == "" || joined[*a.Name] {
				return scenarioInvalid("join")
			}
			joined[*a.Name] = true
		}
		if a.Spec != nil {
			depth, count := 1, 0
			if a.Spec.Task != nil {
				depth, count = 2, 1
			}
			if err := s.scenarioValidateSpec(*a.Spec, depth, &count); err != nil {
				return err
			}
		}
		if a.Items != nil {
			count := 0
			for _, sp := range *a.Items {
				if err := s.scenarioValidateSpec(sp, 2, &count); err != nil {
					return err
				}
			}
		}
	}
	for name := range async {
		if !joined[name] {
			return scenarioInvalid("unjoined async")
		}
	}
	if len(s.Script) == 0 {
		return scenarioInvalid("ending close")
	}
	last := s.Script[len(s.Script)-1]
	if !(last.Action == ActionClose && last.Async == nil) && !(last.Action == ActionJoin && async[*last.Name] == ActionClose) {
		return scenarioInvalid("ending close")
	}
	return scenarioValidateExpect(s.Expect)
}

func (s *Scenario) scenarioValidateSpec(sp Spec, depth int, count *int) error {
	*count = *count + 1
	if depth > s.Limits.MaxDepth || *count > s.Limits.MaxNodes {
		return scenarioInvalid("subtree bounds")
	}
	if (sp.Task == nil) == (sp.Batch == nil) {
		return scenarioInvalid("spec shape")
	}
	if t := sp.Task; t != nil {
		if _, ok := s.Payloads[t.PayloadRef]; !ok {
			return scenarioInvalid("payload reference")
		}
		if t.TimeoutMS != nil {
			if _, err := scenarioDuration(*t.TimeoutMS, false); err != nil {
				return err
			}
		}
		return nil
	}
	b := sp.Batch
	if b.Mode != jobbatch.ModeParallel && b.Mode != jobbatch.ModeSequential {
		return scenarioInvalid("mode")
	}
	if b.OnFail != nil && *b.OnFail != "" && (b.Mode != jobbatch.ModeSequential || (*b.OnFail != jobbatch.OnFailStop && *b.OnFail != jobbatch.OnFailContinue)) {
		return scenarioInvalid("on_fail")
	}
	if b.Notify != nil && *b.Notify != "" && *b.Notify != jobbatch.NotifyAll && *b.Notify != jobbatch.NotifyEach {
		return scenarioInvalid("notify")
	}
	if b.MaxParallel != nil && (*b.MaxParallel < 0 || (b.Mode == jobbatch.ModeSequential && *b.MaxParallel > 1)) {
		return scenarioInvalid("max_parallel")
	}
	for _, child := range b.Items {
		if err := s.scenarioValidateSpec(child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scenario) ConvertSpec(sp Spec) (jobbatch.Spec, error) {
	count := 0
	if err := s.scenarioValidateSpec(sp, 1, &count); err != nil {
		return jobbatch.Spec{}, err
	}
	return s.scenarioConvertSpec(sp)
}

func (s *Scenario) scenarioConvertSpec(sp Spec) (jobbatch.Spec, error) {
	if t := sp.Task; t != nil {
		p := s.Payloads[t.PayloadRef]
		data, err := json.Marshal(p)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		task := &jobbatch.TaskSpec{Kind: p.Kind, Payload: data}
		if t.Group != nil {
			task.Group = *t.Group
		}
		if t.TimeoutMS != nil {
			task.Timeout = time.Duration(*t.TimeoutMS) * time.Millisecond
		}
		return jobbatch.Spec{Task: task}, nil
	}
	b := sp.Batch
	batch := &jobbatch.BatchSpec{Mode: b.Mode, Notify: jobbatch.NotifyAll}
	if b.Mode == jobbatch.ModeSequential {
		batch.OnFail = jobbatch.OnFailStop
	}
	if b.OnFail != nil && *b.OnFail != "" {
		batch.OnFail = *b.OnFail
	}
	if b.Notify != nil && *b.Notify != "" {
		batch.Notify = *b.Notify
	}
	if b.MaxParallel != nil {
		batch.MaxParallel = *b.MaxParallel
	}
	batch.Items = make([]jobbatch.Spec, len(b.Items))
	for i, child := range b.Items {
		converted, err := s.scenarioConvertSpec(child)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		batch.Items[i] = converted
	}
	return jobbatch.Spec{Batch: batch}, nil
}

func (s *Scenario) SubmitRequest(a Action) (jobbatch.SubmitRequest, error) {
	if err := ValidateScenario(s); err != nil {
		return jobbatch.SubmitRequest{}, err
	}
	if a.Action != ActionSubmit {
		return jobbatch.SubmitRequest{}, scenarioInvalid("submit action")
	}
	if err := scenarioValidateAction(a); err != nil {
		return jobbatch.SubmitRequest{}, err
	}
	depth, count := 1, 0
	if a.Spec.Task != nil {
		depth, count = 2, 1
	}
	if err := s.scenarioValidateSpec(*a.Spec, depth, &count); err != nil {
		return jobbatch.SubmitRequest{}, err
	}
	sp, err := s.ConvertSpec(*a.Spec)
	if err != nil {
		return jobbatch.SubmitRequest{}, err
	}
	if sp.Task != nil {
		sp = jobbatch.OneItem(*sp.Task)
	}
	return jobbatch.SubmitRequest{Root: *a.Root, Ref: *a.Ref, Spec: sp}, nil
}

func (s *Scenario) AddRequest(a Action) (jobbatch.AddRequest, error) {
	if err := ValidateScenario(s); err != nil {
		return jobbatch.AddRequest{}, err
	}
	if a.Action != ActionAdd {
		return jobbatch.AddRequest{}, scenarioInvalid("add action")
	}
	if err := scenarioValidateAction(a); err != nil {
		return jobbatch.AddRequest{}, err
	}
	count := 0
	for _, sp := range *a.Items {
		if err := s.scenarioValidateSpec(sp, 2, &count); err != nil {
			return jobbatch.AddRequest{}, err
		}
	}
	out := jobbatch.AddRequest{Parent: *a.Parent, Ref: *a.Ref, Items: make([]jobbatch.Spec, len(*a.Items))}
	for i, sp := range *a.Items {
		converted, err := s.ConvertSpec(sp)
		if err != nil {
			return jobbatch.AddRequest{}, err
		}
		out.Items[i] = converted
	}
	return out, nil
}

func (s *Scenario) ResolveMessage(name string) (json.RawMessage, error) {
	msg, ok := s.Messages[name]
	if !ok || !json.Valid(msg) {
		return nil, scenarioInvalid("message reference")
	}
	return append(json.RawMessage(nil), msg...), nil
}

func scenarioGateValid(r GateResult) bool {
	return r == GateOK || r == GatePanic || strings.HasPrefix(string(r), "error:")
}
