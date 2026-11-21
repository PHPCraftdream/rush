package batchinst

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

const (
	agentToolName     = "agent"
	DefaultAgentDepth = 2
)

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
		if x.Tool == agentToolName {
			if err := validateAgentRoleInput(x.Input); err != nil {
				return jobbatch.Spec{}, err
			}
		}
		if x.Tool == agentToolName || x.Tool == "agentic_fetch" {
			// The runner consumes the edge when it creates the child.
			task := ExternalDepthPayload{Tool: x.Tool, Input: x.Input, AgentDepth: depth}
			payload, marshalErr := json.Marshal(task)
			if marshalErr != nil {
				return jobbatch.Spec{}, marshalErr
			}
			return jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: payload}}, nil
		}
		raw, err := json.Marshal(struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		}{x.Tool, x.Input})
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
		item, err := child.specAt(depth)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		b.Items = append(b.Items, item)
	}
	return jobbatch.Spec{Batch: b}, nil
}

func ParseBatchExternalSpecJSON(raw []byte) (jobbatch.Spec, error) {
	return parseBatchExternalSpecJSON(raw, false, DefaultAgentDepth)
}

func ParseBatchExternalSpecJSONWithAgentDepth(raw []byte, trusted int) (jobbatch.Spec, error) {
	return parseBatchExternalSpecJSON(raw, true, trusted)
}

func parseBatchExternalSpecJSON(raw []byte, withTrustedDepth bool, trusted int) (jobbatch.Spec, error) {
	var input BatchExternalSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
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

type ExternalDepthPayload struct {
	Tool       string          `json:"tool"`
	Input      json.RawMessage `json:"input"`
	AgentDepth int             `json:"agent_depth"`
}

type TimeoutIntent struct {
	Kind     string
	Seconds  int
	Duration time.Duration
}

const (
	TimeoutWakeOnly         = "wake_only"
	TimeoutTerminateAndWake = "terminate_and_wake"
)

const (
	batchTimeoutSecondsFloor = 5
	batchTimeoutSecondsCeil  = 604800 // 7 days
)

type batchTimeoutParamInput struct {
	Timeout        json.RawMessage `json:"timeout,omitempty"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
}

// ParseTimeoutParam normalizes bash, run_command and agent timeouts. A bare
// number means wake_only; legacy timeout_seconds means terminate_and_wake.
// Supplying both is ambiguous. Malformed outer input belongs to tool validation.
func ParseTimeoutParam(toolName, input string) (*TimeoutIntent, error) {
	if toolName != tools.BashToolName && toolName != tools.RunCommandToolName && toolName != agentToolName {
		return nil, nil
	}
	var parsed batchTimeoutParamInput
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil, nil
	}
	raw := bytes.TrimSpace(parsed.Timeout)
	timeoutSet := len(raw) > 0 && string(raw) != "null"
	if timeoutSet && parsed.TimeoutSeconds != 0 {
		return nil, fmt.Errorf("provide at most one of timeout or the legacy timeout_seconds, not both")
	}
	if !timeoutSet {
		if parsed.TimeoutSeconds == 0 {
			return nil, nil
		}
		return &TimeoutIntent{
			Kind:     TimeoutTerminateAndWake,
			Seconds:  parsed.TimeoutSeconds,
			Duration: time.Duration(parsed.TimeoutSeconds) * time.Second,
		}, nil
	}
	const shape = `timeout must be an object {"seconds": N, "kind": "wake_only"|"terminate_and_wake"} or a bare number of seconds`
	var (
		seconds int
		kind    string
	)
	if raw[0] == '{' {
		var obj struct {
			Seconds int    `json:"seconds"`
			Kind    string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("%s: %s", shape, err)
		}
		seconds, kind = obj.Seconds, obj.Kind
	} else {
		if err := json.Unmarshal(raw, &seconds); err != nil {
			return nil, fmt.Errorf("%s, got %s", shape, raw)
		}
		// Bare number: the safe kind -- the deadline never kills the work.
		kind = TimeoutWakeOnly
	}
	if seconds < batchTimeoutSecondsFloor || seconds > batchTimeoutSecondsCeil {
		return nil, fmt.Errorf("timeout.seconds must be between %d and %d (7 days), got %d", batchTimeoutSecondsFloor, batchTimeoutSecondsCeil, seconds)
	}
	switch kind {
	case TimeoutWakeOnly, TimeoutTerminateAndWake:
	default:
		return nil, fmt.Errorf("timeout.kind must be %q or %q, got %q", TimeoutWakeOnly, TimeoutTerminateAndWake, kind)
	}
	return &TimeoutIntent{
		Kind:     kind,
		Seconds:  seconds,
		Duration: time.Duration(seconds) * time.Second,
	}, nil
}
