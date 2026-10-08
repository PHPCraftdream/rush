package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type timeoutBoundaryCoordinator struct {
	agent.Coordinator
	agent.ReactionDebtSource
	run turnRunFunc
}

var _ agent.ReactionDebtSource = (*timeoutBoundaryCoordinator)(nil)

func (c *timeoutBoundaryCoordinator) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, sessionID, prompt)
}

func (c *timeoutBoundaryCoordinator) RunWithOverrides(ctx context.Context, sessionID, prompt string, _, _ *agent.ModelOverride, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, sessionID, prompt)
}

// Revert-check: removing finish's live-context guard changes the empty reviewer outcome from error to canceled.
func TestExecuteRunReviewerDeadlineClassification(t *testing.T) {
	for _, owned := range []bool{false, true} {
		name := "inner-live-context"
		if owned {
			name = "owned-timeout"
		}
		t.Run(name, func(t *testing.T) {
			h := newReviewerPassApp(t, true)
			sess := createModelOverrideSession(t, h.app, "reviewer-deadline")
			ctx := t.Context()
			cause := &agent.RunTimeoutCause{Duration: 3 * time.Second, Source: "--timeout"}
			if owned {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeoutCause(ctx, cause.Duration, cause)
				defer cancel()
			}
			observed := make(chan error, 1)
			h.app.AgentCoordinator = &timeoutBoundaryCoordinator{
				Coordinator:        h.app.AgentCoordinator,
				ReactionDebtSource: h.app.AgentCoordinator.(agent.ReactionDebtSource),
				run: func(turnCtx context.Context, _, _ string) (*fantasy.AgentResult, error) {
					if owned {
						<-turnCtx.Done()
					}
					observed <- turnCtx.Err()
					return nil, context.DeadlineExceeded
				},
			}
			res, err := h.app.ExecuteRun(ctx, RunRequest{
				Prompt: "review", ContinueSessionID: sess.ID, Mode: RunModeJSON,
				Overrides: RunOverrides{ModelRole: config.SelectedModelTypeReviewer},
				Stdout:    io.Discard, Stderr: io.Discard, HideSpinner: true, reviewerTurn: true,
			})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotNil(t, res)
			var timeoutErr *RunTimeoutError
			if owned {
				require.ErrorIs(t, <-observed, context.DeadlineExceeded)
				require.ErrorAs(t, err, &timeoutErr)
				require.Same(t, cause, timeoutErr.Cause)
				require.Equal(t, "timeout", res.ExitReason)
				require.Equal(t, "rush run --role reviewer --session "+sess.ID, res.ResumeCommand)
				require.Equal(t, res.ResumeCommand, timeoutErr.ResumeCommand)
			} else {
				require.NoError(t, <-observed)
				require.NoError(t, ctx.Err())
				require.False(t, errors.As(err, &timeoutErr))
				require.Equal(t, "error", res.ExitReason)
				require.Empty(t, res.ResumeCommand)
			}
			msgs, listErr := h.app.Messages.List(t.Context(), sess.ID)
			require.NoError(t, listErr)
			for _, msg := range msgs {
				require.NotEqual(t, message.Assistant, msg.Role, "no persisted finish may mask isCanceled")
			}
		})
	}
}

// Revert-check: forcing runResumeRole's nonempty branch false loses fast/reviewer in both timeout exit envelopes and errors.
func TestRunTimeoutResumeRoleExitBoundaries(t *testing.T) {
	for _, boundary := range []string{"execute", "cli-loop"} {
		for _, role := range []config.SelectedModelType{"", config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeReviewer} {
			t.Run(boundary+"/"+string(role), func(t *testing.T) {
				h := newReviewerPassApp(t, true)
				sess := createModelOverrideSession(t, h.app, "timeout-resume-role")
				entered := make(chan struct{}, 1)
				h.app.AgentCoordinator = &timeoutBoundaryCoordinator{
					Coordinator:        h.app.AgentCoordinator,
					ReactionDebtSource: h.app.AgentCoordinator.(agent.ReactionDebtSource),
					run: func(ctx context.Context, _, _ string) (*fantasy.AgentResult, error) {
						entered <- struct{}{}
						<-ctx.Done()
						return nil, ctx.Err()
					},
				}
				cause := &agent.RunTimeoutCause{Duration: 3 * time.Second, Source: "--timeout"}
				ctx, cancel := context.WithTimeoutCause(t.Context(), cause.Duration, cause)
				defer cancel()
				var out bytes.Buffer
				var res *RunResult
				var err error
				if boundary == "execute" {
					res, err = h.app.ExecuteRun(ctx, RunRequest{
						Prompt: "do it", ContinueSessionID: sess.ID, Mode: RunModeJSON,
						Overrides: RunOverrides{ModelRole: role}, Stdout: &out, Stderr: io.Discard, HideSpinner: true,
					})
				} else {
					res, err = h.app.RunNonInteractiveWithResult(ctx, &out, "do it", RunOverrides{ModelRole: role, Origin: message.OriginCLI}, true, RunModeJSON, sess.ID, false)
				}
				require.Len(t, entered, 1, "timeout must reach the turn, not fail during setup")
				wantRole := string(role)
				if role == "" {
					wantRole = "smart"
				}
				wantResume := "rush run --role " + wantRole + " --session " + sess.ID
				var timeoutErr *RunTimeoutError
				require.ErrorAs(t, err, &timeoutErr)
				require.Same(t, cause, timeoutErr.Cause)
				require.NotNil(t, res)
				require.Equal(t, "timeout", res.ExitReason)
				require.Equal(t, wantResume, res.ResumeCommand)
				require.Equal(t, wantResume, timeoutErr.ResumeCommand)
				if boundary == "cli-loop" {
					var wire RunResult
					require.NoError(t, json.Unmarshal(out.Bytes(), &wire))
					require.Equal(t, "timeout", wire.ExitReason)
					require.Equal(t, wantResume, wire.ResumeCommand)
				}
			})
		}
	}
}

type timeoutFlushWriter struct {
	err    error
	writes int
}

func (w *timeoutFlushWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, w.err
}

// Revert-check: returning err instead of errors.Join(err, flushErr) drops the writer sentinel from the owned-timeout exit.
func TestRunTimeoutExitJoinsFlushFailure(t *testing.T) {
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "timeout-flush")
	entered := make(chan struct{}, 1)
	h.app.AgentCoordinator = &timeoutBoundaryCoordinator{
		Coordinator:        h.app.AgentCoordinator,
		ReactionDebtSource: h.app.AgentCoordinator.(agent.ReactionDebtSource),
		run: func(ctx context.Context, _, _ string) (*fantasy.AgentResult, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	cause := &agent.RunTimeoutCause{Duration: 3 * time.Second, Source: "--timeout"}
	ctx, cancel := context.WithTimeoutCause(t.Context(), cause.Duration, cause)
	defer cancel()
	flushErr := errors.New("timeout envelope writer failed")
	out := &timeoutFlushWriter{err: flushErr}
	res, err := h.app.RunNonInteractiveWithResult(ctx, out, "do it", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sess.ID, false)
	require.Len(t, entered, 1)
	require.Equal(t, 1, out.writes, "the real exit funnel attempts exactly one JSON flush")
	var timeoutErr *RunTimeoutError
	require.ErrorAs(t, err, &timeoutErr)
	require.ErrorIs(t, err, flushErr)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Same(t, cause, timeoutErr.Cause)
	require.NotNil(t, res)
	require.Equal(t, "timeout", res.ExitReason)
	require.Equal(t, "rush run --role smart --session "+sess.ID, res.ResumeCommand)
	require.Equal(t, res.ResumeCommand, timeoutErr.ResumeCommand)
	require.Equal(t, "timeout", sessionEndedReason(t, h.app, sess.ID))
}
