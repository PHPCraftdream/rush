// Batch rehome (H4 lane P5, phase A): the pacing taxonomy moved to
// runtime/exec/drainctl/turn_arbiter_state.go; these aliases keep the agent
// package's existing spellings for its callers.
package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
)

type drainPace = drainctl.Pace

const (
	paceUncounted     = batchlife.PaceUncounted
	paceFreeNoTurn    = drainctl.PaceFreeNoTurn
	pacePaidUnreacted = drainctl.PacePaidUnreacted
)
