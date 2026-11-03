package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestParseBatchExternalSpecJSON(t *testing.T) {
	spec, err := ParseBatchExternalSpecJSON([]byte(`{"mode":"sequential","items":[{"tool":"agent","input":{"prompt":"p"}}]}`))
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Equal(t, jobbatch.ModeSequential, spec.Batch.Mode)
	require.Equal(t, jobbatch.OnFailStop, spec.Batch.OnFail, "a sequential batch defaults to on_fail stop")
	require.Equal(t, jobbatch.NotifyAll, spec.Batch.Notify)
	require.Len(t, spec.Batch.Items, 1)
	require.NotNil(t, spec.Batch.Items[0].Task)

	spec, err = ParseBatchExternalSpecJSON([]byte(`{"items":[]}`))
	require.NoError(t, err)
	require.Equal(t, jobbatch.ModeParallel, spec.Batch.Mode, "mode defaults to parallel")

	for name, raw := range map[string]string{
		"unknown field":         `{"mode":"parallel","bogus":1}`,
		"trailing value":        `{"mode":"parallel"} {"mode":"parallel"}`,
		"task mixed with batch": `{"tool":"agent","mode":"parallel"}`,
		"task with items":       `{"tool":"agent","items":[{"tool":"agent"}]}`,
		"parallel on_fail":      `{"mode":"parallel","on_fail":"stop"}`,
		"nested bad leaf":       `{"items":[{"tool":"agent","mode":"parallel"}]}`,
		"not json":              `mode: parallel`,
	} {
		_, err = ParseBatchExternalSpecJSON([]byte(raw))
		require.Error(t, err, name)
	}
}
