// Loop-level rate-limit coverage continued: continuation prompts, queued
// admission, the armed-stall-clock invariant, and the wait WARN log. The
// tests shrink package-level timing vars, so NONE may use t.Parallel().
package agent

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunInternal_RateLimitContinuationPrompt: the rate-limit leg keeps the
// continuation branch — a partial-content 429 attempt's retry goes through
// shouldContinueTurn, so the next call's prompt is a continuation prompt.
//
// Revert-check: hoisting the `attempt > maxRetries { break }` (or the
// transient-only continuation) above the rate-limit branch so a 429 never
// continues — the second prompt stays the original and this test goes red.
func TestRunInternal_RateLimitContinuationPrompt(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	partial := []message.ContentPart{message.TextContent{Text: "partial answer so far"}}
	res, calls, prompts, err := rateLimitLoopRun(t, t.Context(), "test-rl-continuation", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if n == 0 {
				return rateLimitAttemptWithParts(t, env, sess, ctx, call, partial, rateLimit429Err())
			}
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 2, calls)
	require.Len(t, prompts, 2)
	hasPrefix := strings.HasPrefix(prompts[1], continuationPromptPrefixPartialText) ||
		strings.HasPrefix(prompts[1], continuationPromptPrefixNoText)
	require.True(t, hasPrefix, "the retry after a partial-content 429 must send a continuation prompt, got %q", prompts[1])
}

// TestRunInternal_RateLimitQueuedAdmissionNotRetried: the R7-1 wasQueued
// break fires before any rate-limit classification, so a queued admission
// with a 429 evidence row is never waited on or retried.
//
// Revert-check: moving the rate-limit branch above the wasQueued break (or
// dropping the break) makes the mock run twice.
func TestRunInternal_RateLimitQueuedAdmissionNotRetried(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	const providerID = "test-rl-queued"
	const prompt = "queued prompt that dies a 429 death"
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	// Default config: maxRetries = 2, so a retried 429 would re-call.
	coord, sess, calls := func() (*coordinator, session.Session, *int) {
		cfg.Config().Providers.Set(providerID, config.ProviderConfig{
			ID: providerID, Type: "openai",
			Models: []catwalk.Model{{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096}},
		})
		sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
		cfg.Config().Models[config.SelectedModelTypeSmart] = sel
		cfg.Config().Models[config.SelectedModelTypeFast] = sel
		coord := &coordinator{
			cfg: cfg, sessions: env.sessions, messages: env.messages,
			modelCache: csync.NewMap[string, cachedModelPair](),
		}
		sess, err := env.sessions.Create(t.Context(), "rl-queued")
		require.NoError(t, err)
		calls := 0
		coord.currentAgent = newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
			calls++
			badRow, err := env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
				Parts: []message.ContentPart{
					message.Finish{Reason: message.FinishReasonError, Message: "Rate limit reached for requests"},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, call.OnAssistantMessageCreated)
			call.OnAssistantMessageCreated(badRow.ID)
			if adm := turnAdmissionFrom(ctx); adm != nil {
				adm.markQueued()
			}
			return nil, nil
		})
		return coord, sess, &calls
	}()
	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	res, err := coord.runInternal(t.Context(), sess.ID, prompt, pinned)
	require.NoError(t, err, "the queued contract must be preserved upward: (nil, nil)")
	assert.Nil(t, res)
	require.Equal(t, 1, *calls, "a queued admission ends the loop before the rate-limit branch ever waits")
}

// TestRunInternal_RateLimitNoArmedStallClock: the rate-limit wait sits
// between attempts with no stall clock armed, so the SUCCESS attempt must
// find no entry in armedStallClocks for the session.
//
// Revert-check: arming a stall clock around the rate-limit wait (instead of
// only inside run()'s turn) makes the in-success-run assertion red.
func TestRunInternal_RateLimitNoArmedStallClock(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	var sessID string
	_, calls, _, err := rateLimitLoopRun(t, t.Context(), "test-rl-no-stall-clock", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if n == 1 {
				sessID = sess.ID
				_, ok := armedStallClocks.Load(sess.ID)
				require.False(t, ok, "no stall clock may be armed during/between rate-limit attempts")
			}
			if n == 0 {
				return rateLimitErrorAttempt(t, env, sess, ctx, call, rateLimit429Err())
			}
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.NotEmpty(t, sessID, "the success invocation must have run")
}

// TestRunInternal_RateLimitWaitWarnLog: the rate-limit branch emits exactly
// its one WARN with attempt=1 and a wait_until=HH:MM field.
//
// Revert-check: removing the slog.Warn("coordinator: provider rate limit,
// waiting before next attempt") call — or logging attempt as the loop's
// attempt counter — turns an assertion red.
func TestRunInternal_RateLimitWaitWarnLog(t *testing.T) {
	shrinkRateLimitVars(t, 5*time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, calls, _, err := rateLimitLoopRun(t, t.Context(), "test-rl-warn-log", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if n == 0 {
				return rateLimitErrorAttempt(t, env, sess, ctx, call, rateLimit429Err())
			}
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	require.Equal(t, 2, calls)

	out := buf.String()
	require.Contains(t, out, "provider rate limit", "the dedicated rate-limit WARN must be logged")
	require.Regexp(t, regexp.MustCompile(`attempt=1\b`), out, "the wait counter, not the transient attempt counter, is logged")
	require.Regexp(t, regexp.MustCompile(`wait_until=\d{2}:\d{2}`), out)
	require.NotContains(t, out, "retrying transient turn failure", "a 429 must not take the transient log line")
}
