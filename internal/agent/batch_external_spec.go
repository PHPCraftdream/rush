package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type BatchExternalSpec = batchinst.BatchExternalSpec

// Keep the CLI and delegation defaults equal until callers share one owner.
var (
	_ [DefaultAgentDepth - batchinst.DefaultAgentDepth]struct{}
	_ [batchinst.DefaultAgentDepth - DefaultAgentDepth]struct{}
)

func ParseBatchExternalSpecJSON(raw []byte) (jobbatch.Spec, error) {
	return batchinst.ParseBatchExternalSpecJSON(raw)
}

func ParseBatchExternalSpecJSONWithAgentDepth(raw []byte, trusted int) (jobbatch.Spec, error) {
	return batchinst.ParseBatchExternalSpecJSONWithAgentDepth(raw, trusted)
}
