package shell

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeXargsArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []string
		want    []string
		wantErr bool
	}{
		{"attached short value", []string{"-n1", "echo"}, []string{"-n", "1", "echo"}, false},
		{"separate short value", []string{"-n", "1", "echo"}, []string{"-n", "1", "echo"}, false},
		{"clustered t and n", []string{"-tn2", "cmd"}, []string{"-t", "-n", "2", "cmd"}, false},
		{"t before separate n", []string{"-t", "-n3", "cmd"}, []string{"-t", "-n", "3", "cmd"}, false},
		{"long max-args equals", []string{"--max-args=3", "cmd"}, []string{"-n", "3", "cmd"}, false},
		{"long max-args separate", []string{"--max-args", "3", "cmd"}, []string{"-n", "3", "cmd"}, false},
		{"long null", []string{"--null", "cmd"}, []string{"-0", "cmd"}, false},
		{"long verbose", []string{"--verbose", "cmd"}, []string{"-t", "cmd"}, false},
		{"double dash ends options", []string{"--", "-n1"}, []string{"--", "-n1"}, false},
		{"boolean cluster", []string{"-0tp", "cmd"}, []string{"-0", "-t", "-p", "cmd"}, false},
		{"bare dash is command", []string{"-", "x"}, []string{"-", "x"}, false},
		{"command args untouched", []string{"echo", "-n1"}, []string{"echo", "-n1"}, false},
		{"unsupported -I untouched", []string{"-I{}", "echo"}, []string{"-I{}", "echo"}, false},
		{"unknown char in cluster leaves token", []string{"-txn2", "cmd"}, []string{"-txn2", "cmd"}, false},
		{"empty args", []string{}, []string{}, false},
		{"zero max args", []string{"-n0"}, nil, true},
		{"negative max args", []string{"-n", "-1"}, nil, true},
		{"missing value", []string{"-n"}, nil, true},
		{"non-numeric", []string{"-nabc"}, nil, true},
		{"n consumes rest of cluster", []string{"-tn2x", "cmd"}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeXargsArgs(tc.in)
			if tc.wantErr {
				require.ErrorIs(t, err, errInvalidXargsN)
				require.Equal(t, xargsInvalidNMessage, err.Error())
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
