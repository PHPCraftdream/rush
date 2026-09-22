package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quotaLimitModel is a fantasy.LanguageModel whose Stream call fails with a
// 429 shaped exactly like a real quota wall (isQuotaLimit's "quota"/"limit
// will reset" substrings), embedding a z.ai-style reset timestamp in the
// provider's OWN timezone (no offset marker -- CST by convention). Only
// Stream is exercised by Run().
type quotaLimitModel struct{}

func (quotaLimitModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, quotaLimitProviderErr
}

func (quotaLimitModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, quotaLimitProviderErr
}

func (quotaLimitModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, quotaLimitProviderErr
}

func (quotaLimitModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, quotaLimitProviderErr
}

func (quotaLimitModel) Provider() string { return "test" }
func (quotaLimitModel) Model() string    { return "quota-limit" }

var quotaLimitProviderErr = &fantasy.ProviderError{
	StatusCode: 429,
	Title:      "rate_limit_exceeded",
	Message:    "Usage limit reached for 5 hour. Your limit will reset at 2026-06-17 14:49:28",
}

// unparseableQuotaLimitModel matches isQuotaLimit's "quota" substring but
// carries no reset hint QuotaLimitResetTime can parse -- the graceful-
// degradation case.
type unparseableQuotaLimitModel struct{}

func (unparseableQuotaLimitModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, unparseableQuotaLimitProviderErr
}

func (unparseableQuotaLimitModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, unparseableQuotaLimitProviderErr
}

func (unparseableQuotaLimitModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, unparseableQuotaLimitProviderErr
}

func (unparseableQuotaLimitModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, unparseableQuotaLimitProviderErr
}

func (unparseableQuotaLimitModel) Provider() string { return "test" }
func (unparseableQuotaLimitModel) Model() string    { return "quota-limit-unparseable" }

var unparseableQuotaLimitProviderErr = &fantasy.ProviderError{
	StatusCode: 429,
	Title:      "rate_limit_exceeded",
	Message:    "You exceeded your quota, contact support",
}

// TestRun_QuotaLimitFinishShowsLocalTime reproduces task #979 end-to-end at
// the real rush run turn-failure surface (handleStreamFailure's
// errors.As(err, &providerErr) branch, agent_turn_failure.go): a
// quota-classified 429 must produce a finish whose Details contains the
// reset time converted to LOCAL time, not the provider's raw remote-zone
// stamp verbatim, while still keeping the original provider message for
// diagnostic value.
func TestRun_QuotaLimitFinishShowsLocalTime(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, quotaLimitModel{}, quotaLimitModel{}, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       sess.ID,
		MaxOutputTokens: 100,
	})
	require.Error(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistant *message.Message
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			assistant = &msgs[i]
		}
	}
	require.NotNil(t, assistant, "expected an assistant message")

	finish := assistant.FinishPart()
	require.NotNil(t, finish)
	assert.Equal(t, message.FinishReasonError, finish.Reason)
	// The raw provider message is kept for diagnostic value...
	assert.Contains(t, finish.Details, "Your limit will reset at 2026-06-17 14:49:28")
	// ...with an ADDED local-time line, not a replacement.
	assert.Contains(t, finish.Details, "Limit resets:")
	// The z.ai stamp has no offset marker -- parsed as fixed CST (UTC+8),
	// then converted to local, exactly like QuotaLimitGuidance's own
	// production code path. Computed independently here (not sliced out
	// of the guidance string) so a format-string drift in production
	// would actually fail this assertion instead of trivially matching.
	cst := time.FixedZone("CST", 8*3600)
	resetAt := time.Date(2026, 6, 17, 14, 49, 28, 0, cst).Local()
	assert.Contains(t, finish.Details, resetAt.Format("2006-01-02 15:04:05 -07:00"), "the reset time must be converted to LOCAL time, not left in the provider's remote zone")
}

// TestRun_QuotaLimitWithoutParseableResetDegradesGracefully covers the
// other half of task #979's acceptance criteria: a quota-classified error
// with NO parseable reset time must fall back to the current
// verbatim-message behavior -- no panic, no empty finish, and no stray
// "Limit resets:" line fabricated from nothing.
func TestRun_QuotaLimitWithoutParseableResetDegradesGracefully(t *testing.T) {
	env := testEnv(t)
	agent := testSessionAgent(env, unparseableQuotaLimitModel{}, unparseableQuotaLimitModel{}, "test system prompt")

	sess, err := env.sessions.Create(t.Context(), "New Session")
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		Prompt:          "hello",
		SessionID:       sess.ID,
		MaxOutputTokens: 100,
	})
	require.Error(t, err)

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)

	var assistant *message.Message
	for i := range msgs {
		if msgs[i].Role == message.Assistant {
			assistant = &msgs[i]
		}
	}
	require.NotNil(t, assistant, "expected an assistant message")

	finish := assistant.FinishPart()
	require.NotNil(t, finish)
	assert.Equal(t, message.FinishReasonError, finish.Reason)
	assert.Equal(t, "You exceeded your quota, contact support", finish.Details, "no parseable reset time -- details must stay exactly the raw provider message")
	assert.NotContains(t, finish.Details, "Limit resets:")
}
