package agent

import "github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"

type (
	pinnedInvoke  = childagent.PinnedInvoke
	cliToolBridge = childagent.CLIBridge
)

var newCLIToolBridgeForContext = childagent.NewCLIBridge
