package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
)

const (
	batchTimeoutWakeOnly         = batchinst.TimeoutWakeOnly
	batchTimeoutTerminateAndWake = batchinst.TimeoutTerminateAndWake
)

func batchParseTimeoutParam(toolName, input string) (*batchinst.TimeoutIntent, error) {
	return batchinst.ParseTimeoutParam(toolName, input)
}
