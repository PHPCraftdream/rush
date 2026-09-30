// A Drain after a long job meets an expired OAuth token (R3B-7). Web and child
// Drains bypass runInternal, so nothing used to refresh the token: the first
// attempt's 401 was terminal and closed the debt. Now the token is refreshed
// before the call is built, the client is rebuilt on it, and a 401 that still
// happens is refreshed at the accounting point and counted like any other
// transient failure. The probe server reports the Authorization header of the
// newest request, so "the call carries a client built after the refresh" is
// observed on the wire. These tests are not parallel: the OAuth fixture
// isolates the global config paths with t.Setenv.
package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// countRefreshes installs a credential refresh and counts its calls: err nil
// succeeds (and leaves new credentials in the config, like the real one), a
// non-nil err fails.
func (f *attemptFixture) countRefreshes(err error) *atomic.Int32 {
	var n atomic.Int32
	f.coord.refreshOAuth2TokenFn = func(context.Context, config.ProviderConfig) error {
		n.Add(1)
		if err == nil {
			f.rotateCredentials(oauthTestProvider)
		}
		return err
	}
	return &n
}

// A 401 followed by a successful credential refresh is a COUNTED, paced
// transient, not a verdict: the next attempt runs on the new credentials.
//
// Revert-check: classifying every 401 terminal again (drainAttemptTerminal
// without the refresh) settles the debt on the first attempt and this test
// goes red.
func TestDrainAttempt_401AfterSuccessfulRefreshIsCountedAndPaced(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-401-refreshed", attemptFixtureOpts{
		noIdle: true, handler: unauthorizedResponse, oauthProvider: oauthTestProvider,
	})
	refreshes := f.countRefreshes(nil)
	f.seedDebt(ctx, "call-1", false)

	_, err := f.drainRun(ctx)
	require.Error(t, err)

	require.EqualValues(t, 1, refreshes.Load(), "the 401 refreshes the credentials once, at the accounting point")
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.WakeAttempts, "counted")
	require.EqualValues(t, 0, row.Reacted, "not a verdict")
	require.EqualValues(t, 0, row.ReactedFailed)
	require.Zero(t, f.markers(ctx))
	require.Equal(t, drainPaced, f.coord.drainPermitted(ctx, f.sessID, false).kind, "paced")
}

// A 401 whose refresh fails (no way to get a working token) stays terminal:
// the first attempt closes the debt with the marker.
//
// Revert-check: treating a failed refresh as success leaves the debt open and
// this test goes red.
func TestDrainAttempt_401WithFailedRefreshSettlesFirstAttempt(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "attempt-401-refresh-fails", attemptFixtureOpts{
		noIdle: true, handler: unauthorizedResponse, oauthProvider: oauthTestProvider,
	})
	refreshes := f.countRefreshes(errors.New("refresh token revoked"))
	f.seedDebt(ctx, "call-1", false)

	_, err := f.drainRun(ctx)
	require.Error(t, err)

	require.EqualValues(t, 1, refreshes.Load())
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.Reacted)
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx))
}

// The Drain call of a root session (web wake, release) is built AFTER an
// expired token was refreshed, and its client carries the new credentials: the
// request on the wire is "Bearer new-key".
//
// Revert-check: dropping the refresh from drainCallFor -- or resolving the
// models before it and not again after -- sends "Bearer old-key" and turns the
// assertions red.
func TestDrainCallFor_RootCallCarriesFreshCredentials(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "drain-call-root", attemptFixtureOpts{
		noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
	})
	refreshes := f.countRefreshes(nil)
	f.seedDebt(ctx, "call-1", false)
	_, err := f.coord.resolveSessionModels(ctx, f.sessID) // a snapshot cached before the refresh
	require.NoError(t, err)

	call, err := f.coord.drainCallFor(ctx, f.sessID)
	require.NoError(t, err)
	require.True(t, call.IsDrain)
	call.Tools = nil // the probe agent's own (empty) toolset
	_, err = f.sa.Run(ctx, call)
	require.NoError(t, err)

	require.EqualValues(t, 1, refreshes.Load(), "an expired token is refreshed before the call is built")
	require.Equal(t, "Bearer new-key", f.authHeader.Load(), "the call carries a client built after the refresh")
}

// A delegated child's Drain call uses its driver's FROZEN model template: its
// client is rebuilt on the current credentials (an expired token refreshed
// first), same model, so the wire carries "Bearer new-key".
//
// Revert-check: dropping freshenDrainClient from drainCallFor leaves the
// template's stale client in the call ("Bearer old-key") and this test goes red.
func TestDrainCallFor_ChildTemplateClientCarriesFreshCredentials(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "drain-call-child", attemptFixtureOpts{noIdle: true, oauthProvider: oauthTestProvider})
	refreshes := f.countRefreshes(nil)
	f.seedDebt(ctx, "call-1", false)
	frozen, err := f.coord.resolveSessionModels(ctx, f.sessID) // the template's client, old credentials
	require.NoError(t, err)
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{
		agent: f.sa, call: SessionAgentCall{SessionID: f.sessID, SmartModel: &frozen.smart},
	})

	call, err := f.coord.drainCallFor(ctx, f.sessID)
	require.NoError(t, err)
	_, err = f.sa.Run(ctx, call)
	require.NoError(t, err)

	require.EqualValues(t, 1, refreshes.Load())
	require.Equal(t, "probe", call.SmartModel.ModelCfg.Model, "same model, new client")
	require.Equal(t, "Bearer new-key", f.authHeader.Load(), "the child's frozen client is replaced")
}

// runInternal resolved the call's model BEFORE its proactive refresh: after a
// refresh the call carries a client built on the new token (a `rush run` Drain
// gets one attempt and must not spend it on the stale client).
//
// Revert-check: dropping the rebuild after the refresh sends "Bearer old-key"
// and this test goes red.
func TestRunInternal_DrainCarriesFreshCredentialsAfterTheProactiveRefresh(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "run-internal-refresh", attemptFixtureOpts{
		noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
	})
	f.coord.currentAgent = f.sa
	refreshes := f.countRefreshes(nil)
	f.seedDebt(ctx, "call-1", false)
	pinned, err := f.coord.resolveSessionModels(ctx, f.sessID) // resolved before the refresh
	require.NoError(t, err)

	_, err = f.coord.runInternal(WithDrainCall(ctx), f.sessID, "", pinned)
	require.NoError(t, err)

	require.EqualValues(t, 1, refreshes.Load())
	require.Equal(t, "Bearer new-key", f.authHeader.Load(), "the call runs on a client rebuilt after the refresh")
}

// withAPIKeyTemplateProvider turns the fixture's provider into an API-key
// template provider (no OAuth token) whose runtime key is `key`.
func (f *attemptFixture) withAPIKeyTemplateProvider(providerID, key string) {
	f.t.Helper()
	pc, ok := f.coord.cfg.Config().Providers.Get(providerID)
	require.True(f.t, ok)
	pc.OAuthToken = nil
	pc.APIKeyTemplate = "$(print-key)"
	pc.APIKey = key
	f.coord.cfg.SetProviderRuntimeConfig(providerID, pc)
}

// A delegated child whose provider uses an API-key TEMPLATE gets its frozen
// client rebuilt too: the 401 accounting re-resolves the template and publishes
// the new key, and the child's next Drain must put that key on the wire (not the
// one its template froze), or two more 401s close the delegation as failed.
//
// Revert-check: restoring the OAuth-only early return in freshenDrainClient
// leaves the frozen client ("Bearer old-key") and turns the wire assertion red.
func TestDrainCallFor_ChildTemplateClientCarriesFreshAPIKey(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "drain-call-child-key", attemptFixtureOpts{noIdle: true, oauthProvider: oauthTestProvider})
	f.withAPIKeyTemplateProvider(oauthTestProvider, "old-key")
	f.seedDebt(ctx, "call-1", false)
	frozen, err := f.coord.resolveSessionModels(ctx, f.sessID) // the template's client, the old key
	require.NoError(t, err)
	f.coord.subAgentDrivers.register(f.sessID, subAgentDriver{
		agent: f.sa, call: SessionAgentCall{SessionID: f.sessID, SmartModel: &frozen.smart},
	})
	f.withAPIKeyTemplateProvider(oauthTestProvider, "new-key") // the 401 accounting's refresh published it

	call, err := f.coord.drainCallFor(ctx, f.sessID)
	require.NoError(t, err)
	_, err = f.sa.Run(ctx, call)
	require.NoError(t, err)

	require.Equal(t, "probe", call.SmartModel.ModelCfg.Model, "same model, new client")
	require.Equal(t, "Bearer new-key", f.authHeader.Load(), "the child's frozen client is replaced")
}

// A per-call credential (a tenant's own key) is never replaced with the shared
// config's, whatever the provider's refreshable credentials are.
//
// Revert-check: dropping the `call.Credentials != nil` guard from
// freshenDrainClient rebuilds the client and turns the identity assertion red.
func TestFreshenDrainClient_PerCallCredentialsKeepTheirClient(t *testing.T) {
	f := newAttemptFixture(t, "drain-call-tenant", attemptFixtureOpts{noIdle: true, oauthProvider: oauthTestProvider})
	f.withAPIKeyTemplateProvider(oauthTestProvider, "shared-key")
	frozen, err := f.coord.resolveSessionModels(context.Background(), f.sessID)
	require.NoError(t, err)
	call := SessionAgentCall{SessionID: f.sessID, SmartModel: &frozen.smart, Credentials: &CredentialSet{}}

	f.coord.freshenDrainClient(context.Background(), &call)

	require.Same(t, &frozen.smart, call.SmartModel, "the tenant's client is kept")
}

func TestHasRefreshableCredential(t *testing.T) {
	require.False(t, hasRefreshableCredential(config.ProviderConfig{APIKey: "static"}))
	require.True(t, hasRefreshableCredential(config.ProviderConfig{APIKeyTemplate: "$(cmd)"}))
	require.True(t, hasRefreshableCredential(config.ProviderConfig{OAuthToken: expiredOAuthToken()}))
	require.False(t, hasRefreshableCredential(config.ProviderConfig{APIKeyTemplate: "literal"}), "a template with no substitution never changes")
}

// shrinkDrainBudgets shrinks the accounting's whole budget and the credential
// refresh's own budget for the calling test (process-wide: not parallel).
func shrinkDrainBudgets(t *testing.T, account, refresh time.Duration) {
	t.Helper()
	oldA, oldR := drainAccountBudgetNS.Swap(int64(account)), drainRefreshBudgetNS.Swap(int64(refresh))
	t.Cleanup(func() { drainAccountBudgetNS.Store(oldA); drainRefreshBudgetNS.Store(oldR) })
}

// R4B-3: the 401 refresh has its own bounded context. With the auth endpoint
// black-holed (the refresh blocks until its context ends) the refresh gives up
// on its own budget and the settle that follows still has the accounting's:
// the unrefreshable 401 closes the row with its marker on the FIRST attempt.
//
// Revert-check: refreshing on the accounting's own context (refreshAfterUnauthorized
// passing ctx through) lets the refresh eat the whole accounting budget; the
// settle then runs on an expired context, closes nothing and turns this red.
func TestDrainAttempt_401BlackHoledRefreshStillSettlesTheRow(t *testing.T) {
	ctx := context.Background()
	shrinkDrainBudgets(t, 3*time.Second, 100*time.Millisecond)
	f := newAttemptFixture(t, "attempt-401-black-hole", attemptFixtureOpts{
		noIdle: true, handler: unauthorizedResponse, oauthProvider: oauthTestProvider,
	})
	var refreshes atomic.Int32
	f.coord.refreshOAuth2TokenFn = func(ctx context.Context, _ config.ProviderConfig) error {
		refreshes.Add(1)
		<-ctx.Done() // TCP open, no answer
		return ctx.Err()
	}
	f.seedDebt(ctx, "call-1", false)

	_, err := f.drainRun(ctx)
	require.Error(t, err)

	require.EqualValues(t, 1, refreshes.Load())
	row := f.row(ctx, "call-1")
	require.EqualValues(t, 1, row.Reacted, "the unrefreshable 401 closes the row at once")
	require.EqualValues(t, 1, row.ReactedFailed)
	require.Equal(t, 1, f.markers(ctx), "with its visible marker")
}
