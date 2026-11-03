package agent

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// BatchExternalSpec is the external structural batch schema shared by
// `rush batch run` and the HTTP batch intake: a batch node (mode, on_fail,
// notify, max_parallel, items) or a task leaf (tool, input).
type BatchExternalSpec struct {
	Mode        string              `json:"mode"`
	OnFail      string              `json:"on_fail,omitempty"`
	Notify      string              `json:"notify,omitempty"`
	MaxParallel int                 `json:"max_parallel,omitempty"`
	Items       []BatchExternalSpec `json:"items,omitempty"`
	Tool        string              `json:"tool,omitempty"`
	Input       json.RawMessage     `json:"input,omitempty"`
}

// Spec translates the external schema into a jobbatch.Spec.
func (x BatchExternalSpec) Spec() (jobbatch.Spec, error) {
	if x.Tool != "" || x.Input != nil {
		if x.Tool == "" || x.Mode != "" || x.OnFail != "" || x.Notify != "" || x.MaxParallel != 0 || len(x.Items) != 0 {
			return jobbatch.Spec{}, errors.New("task and batch fields cannot be mixed")
		}
		raw, err := json.Marshal(BatchCLITask{Tool: x.Tool, Input: x.Input})
		if err != nil {
			return jobbatch.Spec{}, err
		}
		return jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: raw}}, nil
	}
	if x.Mode == "" {
		x.Mode = string(jobbatch.ModeParallel)
	}
	if x.OnFail == "" && x.Mode == string(jobbatch.ModeSequential) {
		x.OnFail = string(jobbatch.OnFailStop)
	}
	if x.Notify == "" {
		x.Notify = string(jobbatch.NotifyAll)
	}
	b := &jobbatch.BatchSpec{Mode: jobbatch.Mode(x.Mode), OnFail: jobbatch.OnFail(x.OnFail), Notify: jobbatch.Notify(x.Notify), MaxParallel: x.MaxParallel}
	if b.Mode == jobbatch.ModeParallel && b.OnFail != "" {
		return jobbatch.Spec{}, errors.New("on_fail is only valid for sequential batches")
	}
	for _, child := range x.Items {
		item, err := child.Spec()
		if err != nil {
			return jobbatch.Spec{}, err
		}
		b.Items = append(b.Items, item)
	}
	return jobbatch.Spec{Batch: b}, nil
}

// ParseBatchExternalSpecJSON decodes one JSON document of the external schema:
// unknown fields and trailing JSON values are errors.
func ParseBatchExternalSpecJSON(raw []byte) (jobbatch.Spec, error) {
	var input BatchExternalSpec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&input); err != nil {
		return jobbatch.Spec{}, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return jobbatch.Spec{}, err
	}
	return input.Spec()
}
