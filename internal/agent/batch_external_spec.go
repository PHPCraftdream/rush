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
	MaxAgentDepth *int                `json:"max_agent_depth,omitempty"`
	Mode          string              `json:"mode"`
	OnFail        string              `json:"on_fail,omitempty"`
	Notify        string              `json:"notify,omitempty"`
	MaxParallel   int                 `json:"max_parallel,omitempty"`
	Items         []BatchExternalSpec `json:"items,omitempty"`
	Tool          string              `json:"tool,omitempty"`
	Input         json.RawMessage     `json:"input,omitempty"`
}

// SpecWithAgentDepth translates an external tree under a trusted caller cap.
func (x BatchExternalSpec) SpecWithAgentDepth(trusted int) (jobbatch.Spec, error) {
	if trusted < 0 || trusted > 32 {
		return jobbatch.Spec{}, errors.New("max_agent_depth must be between 0 and 32")
	}
	depth := trusted
	if x.MaxAgentDepth != nil {
		if *x.MaxAgentDepth < 0 || *x.MaxAgentDepth > trusted {
			return jobbatch.Spec{}, errors.New("max_agent_depth exceeds trusted caller cap")
		}
		depth = *x.MaxAgentDepth
	}
	return x.specAt(depth)
}

// Spec translates the external schema using the CLI default cap.
func (x BatchExternalSpec) Spec() (jobbatch.Spec, error) {
	depth := DefaultAgentDepth
	if x.MaxAgentDepth != nil {
		depth = *x.MaxAgentDepth
		if depth < 0 || depth > 32 {
			return jobbatch.Spec{}, errors.New("max_agent_depth must be between 0 and 32")
		}
	}
	return x.specAt(depth)
}

func (x BatchExternalSpec) specAt(depth int) (jobbatch.Spec, error) {
	if depth < 0 || depth > 32 {
		return jobbatch.Spec{}, errors.New("max_agent_depth must be between 0 and 32")
	}
	if x.MaxAgentDepth != nil {
		if *x.MaxAgentDepth < 0 || *x.MaxAgentDepth > depth {
			return jobbatch.Spec{}, errors.New("nested max_agent_depth may only narrow the inherited cap")
		}
		depth = *x.MaxAgentDepth
	}
	if x.Tool != "" || x.Input != nil {
		if x.Tool == "" || x.Mode != "" || x.OnFail != "" || x.Notify != "" || x.MaxParallel != 0 || len(x.Items) != 0 {
			return jobbatch.Spec{}, errors.New("task and batch fields cannot be mixed")
		}
		if x.Tool == AgentToolName {
			if err := validateAgentRoleInput(x.Input); err != nil {
				return jobbatch.Spec{}, err
			}
		}
		raw, err := json.Marshal(BatchCLITask{Tool: x.Tool, Input: x.Input})
		if err != nil {
			return jobbatch.Spec{}, err
		}
		if x.Tool == "agent" || x.Tool == "agentic_fetch" {
			// The runner consumes the edge when it creates the child.
			task := batchExternalDepthPayload{Tool: x.Tool, Input: x.Input, AgentDepth: depth}
			payload, marshalErr := json.Marshal(task)
			if marshalErr != nil {
				return jobbatch.Spec{}, marshalErr
			}
			return jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: payload}}, nil
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
		childDepth := depth
		if child.MaxAgentDepth != nil {
			if *child.MaxAgentDepth < 0 || *child.MaxAgentDepth > childDepth {
				return jobbatch.Spec{}, errors.New("nested max_agent_depth may only narrow the inherited cap")
			}
			childDepth = *child.MaxAgentDepth
		}
		item, err := child.specAt(childDepth)
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
	return parseBatchExternalSpecJSON(raw, false, DefaultAgentDepth)
}

func ParseBatchExternalSpecJSONWithAgentDepth(raw []byte, trusted int) (jobbatch.Spec, error) {
	return parseBatchExternalSpecJSON(raw, true, trusted)
}

func parseBatchExternalSpecJSON(raw []byte, withTrustedDepth bool, trusted int) (jobbatch.Spec, error) {
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
	if withTrustedDepth {
		return input.SpecWithAgentDepth(trusted)
	}
	return input.Spec()
}
