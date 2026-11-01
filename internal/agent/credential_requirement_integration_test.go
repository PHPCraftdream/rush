package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCredentialedRootSurvivesRestartDrainRefused(t *testing.T) {
	// Revert check: omitting the durable Has lookup from the no-driver drain
	// branch lets the restarted coordinator resolve the configured probe model.
	f := newAttemptFixture(t, "credentialed-restart-drain", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.withOAuthProvider("probe")
	wakeStore := session.NewWakeScheduleStore(f.env.conn)
	_, err := wakeStore.CreateSchedule(context.Background(), session.CreateWakeScheduleParams{
		Owner: f.sessID, Kind: session.WakeKindOnce, Message: "preexisting follow-up", RunAt: time.Now().Add(time.Hour),
	}, time.Now())
	require.NoError(t, err)
	store := session.NewService(db.New(f.env.conn), f.env.conn).(session.SessionCredentialRequirementStore)
	f.coord.credentialReqs = store
	f.coord.currentAgent = &mockSessionAgent{runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		if setup := admissionSetupFrom(ctx); setup != nil {
			if err := setup(ctx); err != nil {
				return nil, err
			}
		}
		return &fantasy.AgentResult{}, nil
	}}
	creds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "tenant-key"}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant", Model: "tenant-model"},
			RoleFast:  {Provider: "tenant", Model: "tenant-model"},
		},
	}
	_, err = f.coord.RunWithCredentials(context.Background(), f.sessID, "prompt", creds)
	require.NoError(t, err)
	require.True(t, f.coord.isCredentialedRoot(f.sessID))

	// An unrelated, never-credentialed root is still allowed through configured
	// model resolution; this guards against a vacuous all-drains-fail result.
	unmarked, err := f.env.sessions.Create(context.Background(), "unmarked-control")
	require.NoError(t, err)
	call, prep, err := f.coord.drainCallFor(context.Background(), unmarked.ID)
	require.NoError(t, err)
	require.True(t, call.IsDrain)
	require.Equal(t, "probe", call.SmartModel.ModelCfg.Model)
	if prep != nil {
		prep.release()
	}

	parent, err := f.env.sessions.Create(context.Background(), "unmarked-parent")
	require.NoError(t, err)
	_, err = f.env.conn.ExecContext(context.Background(), `UPDATE sessions SET parent_session_id = ? WHERE id = ?`, parent.ID, f.sessID)
	require.NoError(t, err)
	child, err := f.env.sessions.CreateTaskSession(context.Background(), "driverless-child", f.sessID, "child")
	require.NoError(t, err)
	sibling, err := f.env.sessions.CreateTaskSession(context.Background(), "unmarked-sibling", parent.ID, "sibling")
	require.NoError(t, err)

	pending, err := wakeStore.ListSchedules(context.Background(), f.sessID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "preexisting scheduled follow-up remains pending")

	// Explicitly simulate restart: retain the fixture's fully initialized
	// coordinator dependencies and shared DB/config, but discard process-local
	// credentialed-root knowledge before exercising persisted requirements.
	f.coord.credentialedRootsMu.Lock()
	f.coord.credentialedRoots = nil
	f.coord.credentialedRootsMu.Unlock()
	restarted := f.coord
	f.requests.Store(0)
	_, _, err = restarted.drainCallFor(context.Background(), f.sessID)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
	_, _, err = restarted.drainCallFor(context.Background(), child.ID)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable, "driverless child inherits its root's durable credential requirement")
	call, prep, err = restarted.drainCallFor(context.Background(), unmarked.ID)
	require.NoError(t, err)
	require.True(t, call.IsDrain)
	require.Equal(t, "probe", call.SmartModel.ModelCfg.Model)
	if prep != nil {
		prep.release()
	}
	call, prep, err = restarted.drainCallFor(context.Background(), sibling.ID)
	require.NoError(t, err)
	require.True(t, call.IsDrain)
	require.Equal(t, "probe", call.SmartModel.ModelCfg.Model)
	if prep != nil {
		prep.release()
	}
	require.Zero(t, f.requests.Load(), "drain model-resolution controls/refusals must not reach the configured provider")
}

func TestCredentialedAdmissionWithoutMarkerStoreStillRefusesInProcessDrain(t *testing.T) {
	f := newAttemptFixture(t, "credentialed-no-marker-store", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.withOAuthProvider("probe")
	f.coord.currentAgent = &mockSessionAgent{runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		if setup := admissionSetupFrom(ctx); setup != nil {
			if err := setup(ctx); err != nil {
				return nil, err
			}
		}
		return &fantasy.AgentResult{}, nil
	}}
	creds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "tenant-key"}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant", Model: "tenant-model"},
			RoleFast:  {Provider: "tenant", Model: "tenant-model"},
		},
	}
	_, err := f.coord.RunWithCredentials(context.Background(), f.sessID, "prompt", creds)
	require.NoError(t, err)
	require.True(t, f.coord.isCredentialedRoot(f.sessID))

	_, _, err = f.coord.drainCallFor(context.Background(), f.sessID)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
	require.Zero(t, f.requests.Load(), "in-memory refusal must not reach the configured provider")
}

func TestCredentialRequirementLookupAndAncestorDriverDrain(t *testing.T) {
	f := newAttemptFixture(t, "credentialed-ancestor-drain", attemptFixtureOpts{noIdle: true, noDriver: true})
	store := session.NewService(db.New(f.env.conn), f.env.conn).(session.SessionCredentialRequirementStore)
	parent, err := f.env.sessions.Get(context.Background(), f.sessID)
	require.NoError(t, err)
	child, err := f.env.sessions.CreateTaskSession(context.Background(), "credentialed-child", parent.ID, "credentialed child")
	require.NoError(t, err)
	require.NoError(t, store.StoreSessionCredentialRequirement(context.Background(), parent.ID))
	f.coord.credentialReqs = store

	_, _, err = f.coord.drainCallFor(context.Background(), child.ID)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable, "unbound child must inherit durable credential requirement")

	childCreds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "tenant-key"}},
		Models:      map[Role]ModelChoice{RoleSmart: {Provider: "tenant", Model: "tenant-model"}},
	}
	f.coord.subAgentDrivers.register(child.ID, subAgentDriver{agent: f.sa, call: SessionAgentCall{SessionID: child.ID, SmartModel: &f.model, Credentials: childCreds}, parentSessionID: parent.ID})
	call, prep, err := f.coord.drainCallFor(context.Background(), child.ID)
	require.NoError(t, err, "credentialed child with its driver binding may drain")
	require.True(t, call.IsDrain)
	if prep != nil {
		prep.release()
	}
}

func TestCredentialRequirementWriteFailureDoesNotMarkInMemoryRoot(t *testing.T) {
	f := newAttemptFixture(t, "credentialed-marker-write-failure", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.withOAuthProvider("probe")
	store := session.NewService(db.New(f.env.conn), f.env.conn).(session.SessionCredentialRequirementStore)
	_, err := f.env.conn.ExecContext(context.Background(), `CREATE TRIGGER fail_credential_marker BEFORE INSERT ON session_run_queue WHEN NEW.id LIKE 'session-credential-requirement:%' BEGIN SELECT RAISE(FAIL, 'marker write failed'); END`)
	require.NoError(t, err)
	f.coord.credentialReqs = store
	f.coord.currentAgent = &mockSessionAgent{runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		if setup := admissionSetupFrom(ctx); setup != nil {
			return nil, setup(ctx)
		}
		return nil, nil
	}}
	creds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "tenant-key"}},
		Models:      map[Role]ModelChoice{RoleSmart: {Provider: "tenant", Model: "tenant-model"}, RoleFast: {Provider: "tenant", Model: "tenant-model"}},
	}
	_, err = f.coord.RunWithCredentials(context.Background(), f.sessID, "prompt", creds)
	require.ErrorContains(t, err, "marker write failed")
	require.False(t, f.coord.isCredentialedRoot(f.sessID), "failed durable admission must not leave an in-memory marker")
	has, lookupErr := store.HasSessionCredentialRequirement(context.Background(), f.sessID)
	require.NoError(t, lookupErr)
	require.False(t, has)
}

func TestCredentialRequirementLookupErrorsFailClosed(t *testing.T) {
	f := newAttemptFixture(t, "credentialed-marker-read-failure", attemptFixtureOpts{noIdle: true, noDriver: true})
	store := session.NewService(db.New(f.env.conn), f.env.conn).(session.SessionCredentialRequirementStore)
	_, err := f.env.conn.ExecContext(context.Background(), `DROP TABLE session_run_queue`)
	require.NoError(t, err)
	f.coord.credentialReqs = store
	require.True(t, f.coord.credentialedSessionOrAncestor(context.Background(), f.sessID))
	_, _, err = f.coord.drainCallFor(context.Background(), f.sessID)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
	require.False(t, errors.Is(err, ErrDiskProviderUnavailable))
}
