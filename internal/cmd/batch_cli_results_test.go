package cmd

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestBatchRunResultIncludesEveryLeafOutcome(t *testing.T) {
	nodes := []db.BatchNode{
		{NodeID: "r", Kind: "batch", State: "failed"},
		{NodeID: "r.1", Kind: "task", State: "completed", OutcomeCode: "ok", OutcomeSummary: "first", ResponseEnvelope: dbString(`{"details":{"data":"alpha"}}`)},
		{NodeID: "r.2", Kind: "task", State: "failed", OutcomeCode: "exit_7", OutcomeSummary: "second", ResponseEnvelope: dbString(`{"details":{"data":"beta"}}`)},
	}
	got := batchLeafResults(nodes, map[string]string{"r.1": "alpha", "r.2": "beta"})
	require.Len(t, got, 2)
	require.Equal(t, "completed", got[0].State)
	require.Equal(t, "ok", got[0].Code)
	require.Equal(t, "first", got[0].Summary)
	require.Equal(t, "alpha", got[0].Output)
	require.True(t, bytes.Contains(got[0].Details, []byte("alpha")))
	require.Equal(t, "failed", got[1].State)
	require.Equal(t, "exit_7", got[1].Code)
	require.Equal(t, "second", got[1].Summary)
	require.Equal(t, "beta", got[1].Output)
	require.True(t, bytes.Contains(got[1].Details, []byte("beta")))
}

func dbString(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
