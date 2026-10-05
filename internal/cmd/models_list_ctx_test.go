package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHumanCtx pins the compact context-window rendering of `rush models
// list`: 1 050 000 must read 1.05M (it read a rounded 1.1M, which looks like a
// different window than the documented one), a round million stays "1M".
//
// Revert-check: format millions with %.1f again and the 1050000 row reads 1.1M.
func TestHumanCtx(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{1_050_000, "1.05M"},
		{1_000_000, "1M"},
		{1_500_000, "1.5M"},
		{2_000_000, "2M"},
		{1_048_576, "1.05M"},
		{272_000, "272k"},
		{999, "999"},
	}
	for _, test := range tests {
		require.Equal(t, test.want, humanCtx(test.in), "humanCtx(%d)", test.in)
	}
}
