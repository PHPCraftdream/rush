package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamCallDoesNotAppendPersistedPromptTwice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		existing  string
		inHistory bool
		want      string
	}{
		{name: "persisted interrupt already in history", existing: "interrupt", inHistory: true, want: ""},
		{name: "referenced message not loaded", existing: "interrupt", want: "operator request"},
		{name: "ordinary new message", want: "operator request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := &turnStream{
				call:       SessionAgentCall{Prompt: "operator request", ExistingMessageID: tc.existing},
				historyIDs: make(map[string]struct{}),
			}
			if tc.inHistory {
				ts.historyIDs[tc.existing] = struct{}{}
			}
			require.Equal(t, tc.want, ts.streamCall(nil, nil, nil).Prompt)
		})
	}
}
