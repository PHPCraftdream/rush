// Revert check: TestSetRateLimitNotify_Fires -> deleting the
// rateLimitNotify Load/Store lines in notifyRateLimitWait/SetRateLimitNotify.
package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSetRateLimitNotify_Fires(t *testing.T) {
	c := &coordinator{}
	var gotID string
	var gotUntil time.Time
	var gotAttempt int
	called := false
	c.SetRateLimitNotify(func(sessionID string, until time.Time, attempt int) {
		called = true
		gotID, gotUntil, gotAttempt = sessionID, until, attempt
	})
	if c.rateLimitNotify.Load() == nil {
		t.Fatal("SetRateLimitNotify did not store the callback")
	}
	until := time.Now().Add(time.Minute)
	c.notifyRateLimitWait("s1", until, 2)
	if !called {
		t.Fatal("notifyRateLimitWait did not invoke the callback")
	}
	if gotID != "s1" || gotAttempt != 2 || !gotUntil.Equal(until) {
		t.Fatalf("got (%q, %v, %d), want (s1, %v, 2)", gotID, gotUntil, gotAttempt, until)
	}
	// Nil install is ignored; unset callback is a no-op.
	c.SetRateLimitNotify(nil)
	c2 := &coordinator{}
	c2.notifyRateLimitWait("s2", until, 1)
}

// TestRunInternal_RateLimitWaitNotifies: each 429 wait in runInternal calls
// the installed notifier once, with the session and a 1-based attempt.
// Revert check: deleting the c.notifyRateLimitWait call in runInternal's
// rate-limit branch (or passing rateLimitWaits instead of +1).
func TestRunInternal_RateLimitWaitNotifies(t *testing.T) {
	shrinkRateLimitVars(t, time.Millisecond, 10*time.Millisecond, time.Second, time.Millisecond, time.Millisecond)
	rlErr := rateLimit429Err()
	var mu sync.Mutex
	var attempts []int
	var ids []string
	var sessID string
	_, calls, _, err := rateLimitLoopRunWith(t, t.Context(), "test-rl-notify", nil,
		func(c *coordinator) {
			c.SetRateLimitNotify(func(sessionID string, until time.Time, attempt int) {
				mu.Lock()
				defer mu.Unlock()
				attempts = append(attempts, attempt)
				ids = append(ids, sessionID)
			})
		},
		func(t *testing.T, env *fakeEnv, sess session.Session, ctx context.Context, n int, call SessionAgentCall) (*fantasy.AgentResult, error) {
			sessID = sess.ID
			if n < 2 {
				return rateLimitErrorAttempt(t, env, sess, ctx, call, rlErr)
			}
			return &fantasy.AgentResult{}, nil
		})
	require.NoError(t, err)
	require.Equal(t, 3, calls)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []int{1, 2}, attempts)
	require.Equal(t, []string{sessID, sessID}, ids)
}
