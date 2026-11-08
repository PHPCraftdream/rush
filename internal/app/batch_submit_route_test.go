package app

// The two submit surfaces must stamp the durable answer-route capability
// differently: the served intake (route=true) keeps held questions working,
// while the standalone unattended CLI surface (route=false) must leave the
// question_route key ABSENT so agent leaves get no ask_question advertisement.
// This test pins that decision end-to-end at the stored payload level.
//
// Fixture rules: the same full-app fixture as the run-command submit tests;
// run_command leaves need no model provider.

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestAppBatchSubmitRouteStampDurablePayload(t *testing.T) {
	a := newFullNewApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Tiny sequential view-leaf-equivalent batch: run_command needs no
	// provider in this fixture. A fresh owner session per submission avoids
	// the owner-session lock conflict.
	runCommand := func() jobbatch.Spec {
		program, args := "sh", []string{"-c", "printf ok"}
		if runtime.GOOS == "windows" {
			program, args = "cmd", []string{"/c", "echo", "ok"}
		}
		return jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeSequential, Items: []jobbatch.Spec{
			{Task: &jobbatch.TaskSpec{Payload: mustJSON(t, map[string]any{
				"tool":  "run_command",
				"input": map[string]any{"program": program, "args": args},
			})}},
		}}}
	}

	readTaskPayload := func(t *testing.T, root string) string {
		t.Helper()
		row := a.DB().QueryRowContext(ctx, `SELECT task_payload FROM batch_nodes WHERE root_id = ? AND kind = 'task'`, root)
		var payload string
		require.NoError(t, row.Scan(&payload))
		return payload
	}

	submitAndWait := func(t *testing.T, owner, root string) {
		t.Helper()
		snapshot, err := a.BatchWait(ctx, owner, root)
		require.NoError(t, err)
		require.Equal(t, string(jobbatch.StateCompleted), snapshot.State)
	}

	t.Run("served intake route=true", func(t *testing.T) {
		sess, err := a.BatchCLISession(ctx, "")
		require.NoError(t, err)
		a.Permissions.AutoApproveSession(sess.ID)
		root, err := a.BatchSubmitRefWithQuestionRoute(ctx, sess.ID, runCommand(), nil, true)
		require.NoError(t, err)
		submitAndWait(t, sess.ID, root)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(readTaskPayload(t, root)), &payload))
		route, ok := payload["question_route"].(bool)
		require.True(t, ok, "served intake payload must carry question_route")
		require.True(t, route)
		require.NoError(t, a.BatchRelease(ctx, sess.ID, root))
	})

	t.Run("unattended intake omits question_route", func(t *testing.T) {
		sess, err := a.BatchCLISession(ctx, "")
		require.NoError(t, err)
		a.Permissions.AutoApproveSession(sess.ID)
		root, err := a.BatchSubmitRefUnattended(ctx, sess.ID, runCommand())
		require.NoError(t, err)
		submitAndWait(t, sess.ID, root)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(readTaskPayload(t, root)), &payload))
		require.NotContains(t, payload, "question_route")
		require.NoError(t, a.BatchRelease(ctx, sess.ID, root))
	})
}
