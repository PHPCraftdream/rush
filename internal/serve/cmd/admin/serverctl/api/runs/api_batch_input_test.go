package apiruns

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestReadAPIBatchSpecReadsStandardInput(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(`{"batch":{}}`))
	for _, args := range [][]string{nil, {"-"}} {
		cmd.SetIn(strings.NewReader(`{"batch":{}}`))
		spec, err := readAPIBatchSpec(cmd, args)
		if err != nil || string(spec) != `{"batch":{}}` {
			t.Fatalf("args=%v spec=%q err=%v", args, spec, err)
		}
	}
	cmd.SetIn(strings.NewReader(strings.Repeat("x", 1<<20+1)))
	if _, err := readAPIBatchSpec(cmd, nil); err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Fatalf("an oversized spec must be refused, err=%v", err)
	}
}
