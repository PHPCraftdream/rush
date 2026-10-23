package lab

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

func labTestScenarioJSON() string {
	return `{"version":1,"limits":{"max_concurrent":2,"max_depth":8,"max_nodes":32},"runner":{"control_timeout_ms":1000,"record_retention":64},"wait_bound_ms":10000,"payloads":{"p":{"kind":"controlled"}},"messages":{"go":{"text":"go"}},"script":[{"action":"submit","ref":"r","root":"R","spec":{"task":{"payload_ref":"p"}}},{"action":"close","expect":"ok"}]}`
}

func TestScenarioStrictValidation(t *testing.T) {
	// V-a/V-e mutation: accepting irrelevant/null fields lets malformed documents reach ingress.
	base := labTestScenarioJSON()
	cases := []string{
		base + ` {}`, strings.Replace(base, `"version":1`, `"version":2`, 1),
		strings.Replace(base, `"version":1`, `"unknown":1,"version":1`, 1),
		strings.Replace(base, `"ref":"r"`, `"ref":""`, 1),
		strings.Replace(base, `"ref":"r"`, `"ref":"bad/ref"`, 1),
		strings.Replace(base, `"ref":"r",`, ``, 1),
		strings.Replace(base, `"expect":"ok"`, `"expect":"ok","ref":"x"`, 1),
		strings.Replace(base, `"expect":"ok"`, `"expect":"ok","ref":null`, 1),
		strings.Replace(base, `"payload_ref":"p"`, `"payload_ref":"absent"`, 1),
		strings.Replace(base, `"payload_ref":"p"`, `"payload_ref":"p","notify":"each"`, 1),
		strings.Replace(base, `"kind":"controlled"`, `"kind":"controlled","program":"x"`, 1),
		strings.Replace(base, `{"action":"close","expect":"ok"}`, `{"action":"stop","node":"R"}`, 1),
		strings.Replace(base, `"wait_bound_ms":10000`, `"wait_bound_ms":9223372036854775807`, 1),
		strings.Replace(base, `"version":1`, `"Version":1`, 1),
		strings.Replace(base, `"kind":"controlled"`, `"kind":"controlled","program":null`, 1),
		strings.Replace(base, `"payload_ref":"p"`, `"payload_ref":"p","group":null`, 1),
	}
	for i, input := range cases {
		if s, err := LoadScenario(strings.NewReader(input)); err == nil || s != nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestScenarioReferencesAndAsync(t *testing.T) {
	// V-a mutation: accepting duplicate refs or unjoined async names bypasses validation.
	base := labTestScenarioJSON()
	prefix := base[:strings.Index(base, `"script":`)]
	scripts := []struct {
		body  string
		valid bool
	}{
		{`[{"action":"close","async":"c"},{"action":"join","name":"c"}]`, true},
		{`[{"action":"close","async":"c"}]`, false},
		{`[{"action":"join","name":"c"},{"action":"close"}]`, false},
		{`[{"action":"close","async":"c"},{"action":"join","name":"c"},{"action":"join","name":"c"}]`, false},
		{`[{"action":"close","async":"c"},{"action":"close","async":"c"},{"action":"join","name":"c"}]`, false},
		{`[{"action":"submit","ref":"x","root":"R","spec":{"task":{"payload_ref":"p"}}},{"action":"add","ref":"x","parent":"R","items":[]},{"action":"close"}]`, false},
		{`[{"action":"await_start","node":"R.1","bind":"a"},{"action":"release_start","attempt":"a","decision":"fail","code":"PRIVATE_SENTINEL/argv=x"},{"action":"close"}]`, true},
		{`[{"action":"complete","attempt":"a","state":"completed"},{"action":"close"}]`, false},
		{`[{"action":"await_start","node":"R.1","bind":"a"},{"action":"await_start","node":"R.2","bind":"a"},{"action":"close"}]`, false},
		{`[{"action":"inject","node":"R.1","message_ref":"missing"},{"action":"close"}]`, false},
		{`[{"action":"close","expect":"ok","expect_error":"closed"}]`, false},
		{`[{"action":"release_start","attempt":"a","decision":"accept","code":""},{"action":"close"}]`, false},
	}
	for i, tc := range scripts {
		_, err := LoadScenario(strings.NewReader(prefix + `"script":` + tc.body + `}`))
		if (err == nil) != tc.valid {
			t.Errorf("case %d validity = %v", i, err == nil)
		}
	}
}

func TestScenarioConversion(t *testing.T) {
	// E1 mutation: applying OneItem to nested/Add leaves changes parent identity.
	s, err := LoadScenario(strings.NewReader(labTestScenarioJSON()))
	if err != nil {
		t.Fatal(err)
	}
	submit, err := s.SubmitRequest(s.Script[0])
	if err != nil {
		t.Fatal(err)
	}
	if submit.Spec.Batch == nil || submit.Spec.Batch.Mode != jobbatch.ModeParallel || submit.Spec.Batch.Notify != jobbatch.NotifyAll || len(submit.Spec.Batch.Items) != 1 || submit.Spec.Batch.Items[0].Task == nil {
		t.Fatal("raw ingress shape")
	}
	group, timeout := "g", int64(12)
	sp := Spec{Batch: &BatchSpec{Mode: jobbatch.ModeSequential, Items: []Spec{{Task: &TaskSpec{PayloadRef: "p", Group: &group, TimeoutMS: &timeout}}}}}
	converted, err := s.ConvertSpec(sp)
	if err != nil {
		t.Fatal(err)
	}
	if converted.Batch.OnFail != jobbatch.OnFailStop || converted.Batch.Items[0].Task == nil || converted.Batch.Items[0].Task.Group != "g" || converted.Batch.Items[0].Task.Timeout.Milliseconds() != 12 {
		t.Fatal("nested conversion")
	}
	ref, parent, items := "add", jobbatch.ID("R"), []Spec{sp.Batch.Items[0]}
	add, err := s.AddRequest(Action{Action: ActionAdd, Ref: &ref, Parent: &parent, Items: &items})
	if err != nil {
		t.Fatal(err)
	}
	if add.Items[0].Task == nil {
		t.Fatal("Add wrapped leaf")
	}
	msg, err := s.ResolveMessage("go")
	if err != nil {
		t.Fatal(err)
	}
	msg[0] = '!'
	if !json.Valid(s.Messages["go"]) {
		t.Fatal("message aliases source")
	}
	converted.Batch.Items[0].Task.Payload[0] = '!'
	again, err := s.ConvertSpec(sp)
	if err != nil || !json.Valid(again.Batch.Items[0].Task.Payload) {
		t.Fatal("payload aliases prior conversion")
	}
}

func TestScenarioFrozenFixturesLoad(t *testing.T) {
	// V-a mutation: loading declared invalid fixtures as valid loses rejection coverage.
	paths, err := filepath.Glob("testdata/scenarios/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatal("scenario fixtures", err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var declared Scenario
			if err := json.Unmarshal(data, &declared); err != nil {
				t.Fatal(err)
			}
			name := strings.TrimSuffix(filepath.Base(path), ".json")
			invalid := strings.HasPrefix(name, "v-a-")
			_, err = LoadScenario(strings.NewReader(string(data)))
			if (err != nil) != invalid {
				t.Fatalf("fixture validity: %s", ScenarioValidationRule(err))
			}
			if invalid {
				t.Logf("invalid: %s", ScenarioValidationRule(err))
			}
		})
	}
}

func TestScenarioSubtreeValidation(t *testing.T) {
	// E1 mutation: omitting helper depth allows a raw root beyond MaxDepth.
	s, err := LoadScenario(strings.NewReader(labTestScenarioJSON()))
	if err != nil {
		t.Fatal(err)
	}
	bad := []Spec{
		{},
		{Task: &TaskSpec{PayloadRef: "p"}, Batch: &BatchSpec{Mode: jobbatch.ModeParallel}},
		{Batch: &BatchSpec{Mode: "unknown"}},
	}
	for _, sp := range bad {
		if _, err := s.ConvertSpec(sp); !errors.Is(err, jobbatch.ErrInvalidSpec) {
			t.Fatal("invalid spec accepted")
		}
	}
	s.Limits.MaxDepth = 1
	if err := ValidateScenario(s); err == nil {
		t.Fatal("raw-root helper depth omitted")
	}
}
