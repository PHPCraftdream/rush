package clibridge

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCLIToolBridgeTranslations(t *testing.T) {
	for _, test := range []struct{ wire, input, want string }{
		{"Read", `{"path":"x"}`, `{"file_path":"x","limit":500}`},
		{"Read", `{"path":"x","start_line":1,"end_line":1}`, `{"file_path":"x","limit":1}`},
		{"Read", `{"path":"x","start_line":2,"end_line":4}`, `{"file_path":"x","offset":1,"limit":3}`},
		{"Read", `{"path":"x","start_line":3}`, `{"file_path":"x","offset":2,"limit":500}`},
		{"Read", `{"path":"x","end_line":2}`, `{"file_path":"x","limit":2}`},
		{"Write", `{"path":"x","content":"data"}`, `{"file_path":"x","content":"data"}`},
		{"Grep", `{"pattern":"p","glob":"*.go","path":"x"}`, `{"pattern":"p","include":"*.go","path":"x"}`},
		{"Glob", `{"pattern":"*.go","path":"x"}`, `{"pattern":"*.go","path":"x"}`},
		{"Bash", `{"command":"echo x","description":"d","working_dir":"x"}`, `{"command":"echo x","description":"d","working_dir":"x"}`},
		{"todos", `{"todos":[]}`, `{"todos":[]}`},
	} {
		t.Run(test.wire+test.input, func(t *testing.T) {
			got, _, err := TranslateCLIInput(test.wire, test.input)
			require.NoError(t, err)
			require.JSONEq(t, test.want, got, "Inclusive boundaries and wire input translation")
		})
	}
	for _, input := range []string{`{"start_line":-1}`, `{"end_line":-1}`, `{"start_line":5,"end_line":4}`} {
		_, _, err := TranslateCLIInput("Read", input)
		require.Error(t, err, "Invalid Read range fails closed")
	}
	for _, key := range []string{"timeout", "run_in_background", "background", "auto_background_after"} {
		_, _, err := TranslateCLIInput("Bash", fmt.Sprintf(`{"command":"echo x",%q:1}`, key))
		require.Error(t, err)
	}
}
