// Regression coverage for the coordinator's dedicated 429-non-quota
// rate-limit wait budget (coordinator_rate_limit.go and the rate-limit leg
// of the retry loop in coordinator_run.go). Each test's header comment names
// the single production line it must catch. The tests shrink package-level
// timing vars, so NONE of them may use t.Parallel().
package agent

import (
	"context"
	"net/http"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// shrinkRateLimitVars replaces the package-level rate-limit timing vars (and
// the transient backoff) with test-sized values. Global state: NEVER call
// from a t.Parallel() test.
func shrinkRateLimitVars(t *testing.T, base, maxWait, budget, tick, backoff time.Duration) {
	t.Helper()
	ob, om, obu, ot, osb := rateLimitBaseWait, rateLimitMaxWait, rateLimitBudget, rateLimitWaitTick, streamStallRetryBaseBackoff
	rateLimitBaseWait, rateLimitMaxWait, rateLimitBudget, rateLimitWaitTick, streamStallRetryBaseBackoff = base, maxWait, budget, tick, backoff
	t.Cleanup(func() {
		rateLimitBaseWait, rateLimitMaxWait, rateLimitBudget, rateLimitWaitTick, streamStallRetryBaseBackoff = ob, om, obu, ot, osb
	})
}

// rateLimit429Err is the canonical non-quota 429 provider error (no quota
// substring, so isQuotaLimit is false).
func rateLimit429Err() *fantasy.ProviderError {
	return &fantasy.ProviderError{
		StatusCode: http.StatusTooManyRequests,
		Title:      "rate_limit_exceeded",
		Message:    "Rate limit reached for requests",
	}
}

// rateLimitLoopRun wires the standard retry-loop harness (testEnv, config
// init, mock agent) with a default-config coordinator whose n-th mock call
// (0-based) is handled by fn. It returns the runInternal outcome plus the
// call count and prompts seen.
func rateLimitLoopRun(t *testing.T, ctx context.Context, providerID string, cfgMut func(*config.Config), fn func(t *testing.T, env *fakeEnv, sess session.Session, callCtx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error)) (*fantasy.AgentResult, int, []string, error) {
	t.Helper()
	return rateLimitLoopRunWith(t, ctx, providerID, cfgMut, nil, fn)
}

// rateLimitLoopRunWith is rateLimitLoopRun with a hook on the coordinator
// before the run starts.
func rateLimitLoopRunWith(t *testing.T, ctx context.Context, providerID string, cfgMut func(*config.Config), coordMut func(*coordinator), fn func(t *testing.T, env *fakeEnv, sess session.Session, callCtx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error)) (*fantasy.AgentResult, int, []string, error) {
	t.Helper()
	env := testEnv(t)
	cfg, err := config.Init(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.Config().Providers.Set(providerID, config.ProviderConfig{
		ID: providerID, Type: "openai",
		Models: []catwalk.Model{{ID: "test-model", Name: "Test Model", DefaultMaxTokens: 4096}},
	})
	sel := config.SelectedModel{Provider: providerID, Model: "test-model"}
	cfg.Config().Models[config.SelectedModelTypeSmart] = sel
	cfg.Config().Models[config.SelectedModelTypeFast] = sel
	if cfgMut != nil {
		cfgMut(cfg.Config())
	}
	// permissions/history/filetracker are required: when cfg.Agents ends up
	// with a coder entry (e.g. after a cfgMut sets Options), resolveSessionModels
	// runs pinCallTools -> buildTools, and buildTools hands c.permissions to
	// tools.NewBashTool — a nil there is a guaranteed panic.
	coord := &coordinator{
		cfg: cfg, sessions: env.sessions, messages: env.messages,
		permissions: env.permissions, history: env.history, filetracker: *env.filetracker,
		modelCache: csync.NewMap[string, cachedModelPair](),
	}
	if coordMut != nil {
		coordMut(coord)
	}
	sess, err := env.sessions.Create(t.Context(), "rl-loop-"+providerID)
	require.NoError(t, err)

	calls := 0
	var prompts []string
	coord.currentAgent = newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		n := calls
		calls++
		prompts = append(prompts, call.Prompt)
		return fn(t, &env, sess, ctx, n, call)
	})
	pinned, err := coord.resolveSessionModels(t.Context(), sess.ID)
	require.NoError(t, err)
	res, err := coord.runInternal(ctx, sess.ID, "do the thing", pinned)
	return res, calls, prompts, err
}

// rateLimitErrorAttempt writes an error-finish, no-progress assistant row,
// fires OnAssistantMessageCreated, and returns a 429 provider error.
func rateLimitErrorAttempt(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, call SessionAgentCall, err429 error) (*fantasy.AgentResult, error) {
	return rateLimitAttemptWithParts(t, env, sess, ctx, call, nil, err429)
}

// rateLimitAttemptWithParts is rateLimitErrorAttempt with extra leading
// content parts (the partial-content continuation shape).
func rateLimitAttemptWithParts(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, call SessionAgentCall, extra []message.ContentPart, err429 error) (*fantasy.AgentResult, error) {
	t.Helper()
	parts := append([]message.ContentPart{}, extra...)
	parts = append(parts, message.Finish{Reason: message.FinishReasonError, Message: "Rate limit reached for requests"})
	row, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant, Parts: parts,
	})
	require.NoError(t, err)
	if call.OnAssistantMessageCreated != nil {
		call.OnAssistantMessageCreated(row.ID)
	}
	return nil, err429
}

// TestClassifyProviderError_RateLimitClass: the non-quota 429 branch of
// classifyProviderError must return classRateLimit (not classTransient,
// not classTerminal), with quota 429 staying terminal.
//
// Revert-check: reverting the 429 non-quota return to classTransient turns
// the first assertion red; reverting isQuotaLimit's classification turns the
// second red.
func TestClassifyProviderError_RateLimitClass(t *testing.T) {
	require.Equal(t, classRateLimit, classifyProviderError(rateLimit429Err()))
	require.Equal(t, classTerminal, classifyProviderError(quotaLimitProviderErr))
	retryErr := &fantasy.RetryError{Errors: []error{rateLimit429Err()}}
	require.Equal(t, classRateLimit, classifyProviderError(retryErr), "errors.As must reach the ProviderError through RetryError")
	require.Equal(t, classTransient, classifyProviderError(&fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable}))
	require.Equal(t, classTerminal, classifyProviderError(&fantasy.ProviderError{StatusCode: http.StatusUnauthorized}))
}

// TestRetryAfter_Table: retryAfter's header parsing — retry-after-ms wins
// over retry-after, seconds and HTTP-date both parse, everything positive is
// capped at rateLimitMaxWait, missing/unparseable/past-date yields false.
//
// Revert-check: removing the retry-after-ms pass (or the cap) flips an
// assertion red.
func TestRetryAfter_Table(t *testing.T) {
	origMax := rateLimitMaxWait
	rateLimitMaxWait = time.Minute
	t.Cleanup(func() { rateLimitMaxWait = origMax })

	pe := func(headers map[string]string) *fantasy.ProviderError {
		return &fantasy.ProviderError{StatusCode: 429, ResponseHeaders: headers}
	}

	d, ok := retryAfter(pe(map[string]string{"retry-after": "30"}))
	require.True(t, ok)
	require.Equal(t, 30*time.Second, d)

	// Revert-check: an exact-key lookup misses net/http's canonical header names.
	d, ok = retryAfter(pe(map[string]string{"Retry-After": "30"}))
	require.True(t, ok)
	require.Equal(t, 30*time.Second, d)
	d, ok = retryAfter(pe(map[string]string{"Retry-After-Ms": "1500", "Retry-After": "30"}))
	require.True(t, ok)
	require.Equal(t, 1500*time.Millisecond, d)

	d, ok = retryAfter(pe(map[string]string{"retry-after-ms": "1500"}))
	require.True(t, ok)
	require.Equal(t, 1500*time.Millisecond, d)

	d, ok = retryAfter(pe(map[string]string{"retry-after-ms": "1500", "retry-after": "30"}))
	require.True(t, ok)
	require.Equal(t, 1500*time.Millisecond, d, "ms key must win when both are present")

	future := time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)
	d, ok = retryAfter(pe(map[string]string{"retry-after": future}))
	require.True(t, ok)
	require.Positive(t, d)
	require.LessOrEqual(t, d, rateLimitMaxWait, "HTTP-date waits are capped too")

	d, ok = retryAfter(pe(map[string]string{"retry-after": "99999"}))
	require.True(t, ok)
	require.Equal(t, rateLimitMaxWait, d, "huge values must be capped")

	_, ok = retryAfter(pe(nil))
	require.False(t, ok)
	_, ok = retryAfter(pe(map[string]string{"x-other": "1"}))
	require.False(t, ok)

	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	_, ok = retryAfter(pe(map[string]string{"retry-after": past}))
	require.False(t, ok, "a past HTTP date is no usable wait")

	_, ok = retryAfter(pe(map[string]string{"retry-after": "not-a-number"}))
	require.False(t, ok)
}

// TestRunInternal_RateLimitRetriesBeyondMaxRetries: the rate-limit leg does
// NOT increment attempt, so 4x429-then-success succeeds even though
// maxRetries is the default 2 (5 provider calls total).
//
// Revert-check: incrementing attempt in the rate-limit branch (or letting
// the 429 hit the transient `attempt > maxRetries` break) stops at 3 calls
// and this test goes red.
func TestRunInternal_RateLimitRetriesBeyondMaxRetries(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	rlErr := rateLimit429Err()
	res, calls, _, err := rateLimitLoopRun(t, t.Context(), "test-rl-beyond-max", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if n < 4 {
				return rateLimitErrorAttempt(t, env, sess, ctx, call, rlErr)
			}
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 5, calls, "4 rate-limit waits + the success call, regardless of maxRetries=2")
}

// TestRunInternal_RateLimitBudgetExhausted: waitRateLimit's
// `*spent >= rateLimitBudget` exhaustion check ends the loop with the
// ORIGINAL 429 error after exactly one call.
//
// Revert-check: ignoring the exhausted error (or surfacing the budget error
// instead of the original 429) turns an assertion red.
func TestRunInternal_RateLimitBudgetExhausted(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, 0, time.Millisecond, time.Millisecond)
	rlErr := rateLimit429Err()
	res, calls, _, err := rateLimitLoopRun(t, t.Context(), "test-rl-budget-zero", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return rateLimitErrorAttempt(t, env, sess, ctx, call, rlErr)
		})
	require.Error(t, err)
	require.ErrorIs(t, err, rlErr, "the ORIGINAL 429 must surface, not the budget error")
	require.Nil(t, res)
	require.Equal(t, 1, calls)
}

// TestRunInternal_RateLimitWaitContextDeadline: the ctx.Done() select inside
// waitRateLimit must surface ctx.Err().
//
// Revert-check: removing the ctx.Done() case from waitRateLimit's select
// makes the wait run to completion and this test goes red.
func TestRunInternal_RateLimitWaitContextDeadline(t *testing.T) {
	shrinkRateLimitVars(t, time.Minute, time.Minute, time.Hour, 10*time.Millisecond, time.Millisecond)
	// The deadline starts at the first provider call: a cold setup slower
	// than the deadline would otherwise expire it before the wait begins.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, calls, _, err := rateLimitLoopRun(t, ctx, "test-rl-ctx-deadline", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, callCtx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			if n == 0 {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			return rateLimitErrorAttempt(t, env, sess, context.WithoutCancel(callCtx), call, rateLimit429Err())
		})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
}

// TestRunInternal_RateLimitDrainIsOneAttempt: the IsDrain maxRetries=0
// override disables the rate-limit wait too.
//
// Revert-check: dropping the `if maxRetries == 0 { break }` inside the
// rate-limit branch makes the Drain wait and re-call — this test goes red.
func TestRunInternal_RateLimitDrainIsOneAttempt(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	rlErr := rateLimit429Err()
	_, calls, _, err := rateLimitLoopRun(t, WithDrainCall(t.Context()), "test-rl-drain", nil,
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return rateLimitErrorAttempt(t, env, sess, ctx, call, rlErr)
		})
	require.Error(t, err)
	require.ErrorIs(t, err, rlErr)
	require.Equal(t, 1, calls, "a Drain is one provider attempt, no rate-limit wait")
}

// TestRunInternal_RateLimitZeroRetriesDisablesWait: explicit
// stream_stall_retries=0 disables the rate-limit wait too.
//
// Revert-check: the `if maxRetries == 0 { break }` in the rate-limit branch
// is what this exercises — remove it and the mock is called twice.
func TestRunInternal_RateLimitZeroRetriesDisablesWait(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	// Keep the existing Options pointer and set only StreamStallRetries:
	// replacing Options wholesale drops Options.Attribution, and when this
	// cfg also resolves the coder agent, buildTools hands that nil
	// attribution to tools.NewBashTool -> bashDescription, which
	// dereferences it (`*attribution`) and panics.
	zero := 0
	rlErr := rateLimit429Err()
	_, calls, _, err := rateLimitLoopRun(t, t.Context(), "test-rl-zero-retries",
		func(cfg *config.Config) {
			if cfg.Options == nil {
				cfg.Options = &config.Options{}
			}
			cfg.Options.StreamStallRetries = &zero
		},
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			return rateLimitErrorAttempt(t, env, sess, ctx, call, rlErr)
		})
	require.ErrorIs(t, err, rlErr)
	require.Equal(t, 1, calls, "stream_stall_retries=0 must disable the rate-limit wait entirely")
}
