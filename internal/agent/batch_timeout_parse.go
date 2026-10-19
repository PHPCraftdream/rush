// Batch-copy timeout parsing: the retained copy of the legacy parser out of
// async_tool.go so H4's deletion of the legacy machinery cannot remove it
// before W2/W3 consumers migrate. Behavior is copied verbatim; names are
// batch-prefixed to avoid collisions with the legacy identifiers.
//
// The parser is the batch ingress' single authority for a tool call's
// timeout intent (wake-tools-contract.md §3): direct tool calls normalize
// here in batchOneItemSpec, explicit batch leaves in modelBatchSpecAt.

package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// batchTimeoutIntent is the parsed timeout intent of one tool call: the
// requested kind (wake_only or terminate_and_wake), the original seconds,
// and the equivalent duration.
type batchTimeoutIntent struct {
	Kind     string
	Seconds  int
	Duration time.Duration
}

const (
	// batchTimeoutWakeOnly is the safe kind: the deadline never kills the
	// work, it only checks in.
	batchTimeoutWakeOnly = "wake_only"
	// batchTimeoutTerminateAndWake latches the engine's termination
	// deadline (TaskSpec.Timeout).
	batchTimeoutTerminateAndWake = "terminate_and_wake"
)

// batchTimeoutSecondsFloor/batchTimeoutSecondsCeil bound timeout.seconds
// (contract: sane minimum, exactly 7 days maximum).
const (
	batchTimeoutSecondsFloor = 5
	batchTimeoutSecondsCeil  = 604800 // 7 days
)

// batchTimeoutParamInput is the generic, tool-agnostic shape the parser
// reads from raw call.Input (the legacy timeoutParamInput, copied).
type batchTimeoutParamInput struct {
	// Raw so a bare-number timeout survives this generic re-parse instead
	// of being silently dropped.
	Timeout json.RawMessage `json:"timeout,omitempty"`
	// TimeoutSeconds mirrors run_command.RunCommandParams.TimeoutSeconds,
	// read generically so this function stays tool-agnostic. Zero for
	// bash/agent (they have no such field).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// batchParseTimeoutParam extracts and validates the optional explicit
// timeout for bash/run_command/agent (NOT agentic_fetch -- its own HTTP
// client already carries a fixed timeout, and it does not describe this
// field in its JSON schema). Any other tool returns (nil, nil). A bare JSON
// number is accepted as {"seconds": N, "kind": "wake_only"}. Both new
// timeout{} and legacy run_command.timeout_seconds set on the SAME call is
// a validation error (wake-tools-contract.md §3): the legacy field is a
// "terminate_and_wake" ALIAS, not an independent axis, so both present is
// an unresolvable ambiguity, not a silent precedence rule. A malformed
// outer input returns (nil, nil) like the legacy parser: the tool's own
// schema validation owns that failure.
func batchParseTimeoutParam(toolName, input string) (*batchTimeoutIntent, error) {
	if toolName != tools.BashToolName && toolName != tools.RunCommandToolName && toolName != AgentToolName {
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
		// Legacy alias: run_command.timeout_seconds's OWN validation
		// (its tools/run_command.go clamps) is untouched and runs
		// separately for the process-kill mechanism itself -- this only
		// records the SAME deadline for the batch engine.
		return &batchTimeoutIntent{
			Kind:     batchTimeoutTerminateAndWake,
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
		kind = batchTimeoutWakeOnly
	}
	if seconds < batchTimeoutSecondsFloor || seconds > batchTimeoutSecondsCeil {
		return nil, fmt.Errorf("timeout.seconds must be between %d and %d (7 days), got %d", batchTimeoutSecondsFloor, batchTimeoutSecondsCeil, seconds)
	}
	switch kind {
	case batchTimeoutWakeOnly, batchTimeoutTerminateAndWake:
	default:
		return nil, fmt.Errorf("timeout.kind must be %q or %q, got %q", batchTimeoutWakeOnly, batchTimeoutTerminateAndWake, kind)
	}
	return &batchTimeoutIntent{
		Kind:     kind,
		Seconds:  seconds,
		Duration: time.Duration(seconds) * time.Second,
	}, nil
}
