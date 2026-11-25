package mailbox

import "github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

// ActiveCallToken is the identity fence for one mailbox generation.
type ActiveCallToken struct {
	Epoch      uint64
	Generation uint64
	Call       *agentcall.SessionAgentCall
}
