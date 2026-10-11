package config

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"net/http"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type oauthMatrixCase struct {
	journal, config, memory, used, storage string
	expiry                                 string
	owned, valid                           bool
}

type oauthMatrixTokens struct {
	a, b, c, earlier, d *oauth.Token
}

type oauthMatrixWant struct {
	calls       int
	predecessor *oauth.Token
	memory      *oauth.Token
	final       *oauth.Token
	finalMemory *oauth.Token
	converges   bool
	blockedIO   bool
}

func TestOAuthRound7_CartesianMatrix(t *testing.T) {
	candidates, executed, skipped := 0, 0, 0
	coverage := make(map[string]int)
	for _, journal := range []string{"absent", "validB", "invalidquarantineOK", "invalidquarantinefail", "transientIO", "persistentIO"} {
		for _, owned := range []bool{false, true} {
			for _, config := range []string{"A", "Bdurable", "Bsyncfail", "laterC", "earlier", "logout", "corrupt"} {
				expiries := []string{"fixed"}
				if config == "A" || config == "laterC" {
					expiries = []string{"earlier", "equal", "later"}
				}
				for _, expiry := range expiries {
					for _, memory := range []string{"A", "B"} {
						for _, used := range []string{"A", "B"} {
							for _, valid := range []bool{false, true} {
								for _, storage := range []string{"healthy", "failthenrepair"} {
									candidate := oauthMatrixCase{journal: journal, config: config, memory: memory, used: used, storage: storage, expiry: expiry, owned: owned, valid: valid}
									candidates++
									t.Run(candidate.name(), func(t *testing.T) {
										executed++
										coverage[config+"/"+expiry]++
										runOAuthMatrixCase(t, candidate)
									})
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("candidates=%d executed=%d skipped=%d", candidates, executed, skipped)
	assert.Equal(t, 2112, candidates)
	assert.Equal(t, 2112, executed)
	assert.Zero(t, skipped)
	for _, config := range []string{"A", "laterC"} {
		for _, expiry := range []string{"earlier", "equal", "later"} {
			assert.Equal(t, 192, coverage[config+"/"+expiry], "expiry coverage %s/%s", config, expiry)
		}
	}
	for _, config := range []string{"Bdurable", "Bsyncfail", "earlier", "logout", "corrupt"} {
		assert.Equal(t, 192, coverage[config+"/fixed"], "fixed coverage %s", config)
	}
	assert.Equal(t, candidates, executed+skipped)
}

func (c oauthMatrixCase) name() string {
	return fmt.Sprintf("journal=%s/outbox=%t/config=%s/expiry=%s/memory=%s/used=%s/Bvalid=%t/storage=%s", c.journal, c.owned, c.config, c.expiry, c.memory, c.used, c.valid, c.storage)
}

func newOAuthMatrixTokens(id string, valid bool) oauthMatrixTokens {
	now := time.Now()
	token := func(name string, expires time.Time) *oauth.Token {
		return &oauth.Token{AccessToken: id + "-" + name + "-access", RefreshToken: id + "-" + name + "-refresh", ExpiresAt: expires.Unix(), ExpiresIn: 3600, AccountID: "matrix-account"}
	}
	bExpiry := now.Add(time.Hour)
	if !valid {
		bExpiry = now.Add(-time.Hour)
	}
	return oauthMatrixTokens{
		a: token("A", now.Add(-3*time.Hour)), b: token("B", bExpiry),
		c: token("C", now.Add(2*time.Hour)), earlier: token("earlier", now.Add(-2*time.Hour)),
		d: token("D", now.Add(3*time.Hour)),
	}
}

// The oracle declares authority and expiry rules without production arbitration.
func oauthMatrixOracle(c oauthMatrixCase, tok oauthMatrixTokens) oauthMatrixWant {
	memory, used := tok.a, tok.a
	if c.memory == "B" {
		memory = tok.b
	}
	if c.used == "B" {
		used = tok.b
	}
	walB := c.journal == "validB" || c.journal == "transientIO" || c.journal == "persistentIO"
	knownPredecessor := c.owned || walB
	disk := oauthMatrixDisk(c, tok)
	readable := c.config != "corrupt"
	authority := memory
	if knownPredecessor {
		authority = tok.b
	}
	// Only a record proves A burned; otherwise disk compares against plain live.
	if readable && disk != nil && !(knownPredecessor && disk == tok.a) && disk.ExpiresAt >= authority.ExpiresAt {
		authority = disk
	}
	finalDisk := disk
	if !readable {
		finalDisk = tok.a // The harness repairs corrupt config to A.
	}
	want := oauthMatrixWant{memory: memory, final: finalDisk, finalMemory: memory, converges: knownPredecessor}
	if knownPredecessor {
		want.final, want.finalMemory = authority, authority
	}
	if c.config == "logout" {
		want.memory, want.final, want.finalMemory = nil, nil, nil
		return want
	}
	fastRelease := !memory.IsExpired() && memory != used
	diskRelease := readable && disk == authority && !disk.IsExpired() && disk != used
	if c.journal == "persistentIO" && !c.owned {
		// Unreadable ownership forbids disk publication, not valid-live release.
		want.blockedIO = !fastRelease
		return want
	}
	if diskRelease {
		want.memory = disk
		if !knownPredecessor {
			want.finalMemory = disk
		}
		return want
	}
	if fastRelease && authority == memory {
		return want
	}
	want.memory = authority
	if !authority.IsExpired() && authority != used {
		if !knownPredecessor {
			want.finalMemory = authority
		}
		return want
	}
	// Earlier disk cannot justify exchanging stale caller A instead of live B.
	want.calls, want.predecessor = 1, authority
	want.memory, want.final, want.finalMemory = tok.d, tok.d, tok.d
	want.converges = true
	return want
}

func oauthMatrixDisk(c oauthMatrixCase, tok oauthMatrixTokens) *oauth.Token {
	switch c.config {
	case "Bdurable", "Bsyncfail":
		return tok.b
	case "laterC":
		return tok.c
	case "earlier":
		return tok.earlier
	case "logout":
		return nil
	default:
		return tok.a
	}
}

func writeOAuthMatrixDisk(t *testing.T, path string, token *oauth.Token) {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"matrix_owned_data": "preserve-me",
		"providers": map[string]any{
			"openai-codex":     map[string]any{"oauth": token, "matrix_provider_data": "preserve-provider"},
			"matrix-unrelated": map[string]any{"api_key": "fixture-only"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func runOAuthMatrixCase(t *testing.T, c oauthMatrixCase) {
	f := newCodexRefreshFixture(t)
	tok := newOAuthMatrixTokens(fmt.Sprintf("m%08x", crc32.ChecksumIEEE([]byte(c.name()))), c.valid)
	delta := int64(0)
	switch c.expiry {
	case "earlier":
		delta = -3600
	case "later":
		delta = 3600
	}
	switch c.config {
	case "A":
		tok.a.ExpiresAt = tok.b.ExpiresAt + delta
	case "laterC":
		tok.c.ExpiresAt = tok.b.ExpiresAt + delta
	}
	want := oauthMatrixOracle(c, tok)
	f.used.OAuthToken = tok.a
	memory := f.used
	if c.memory == "B" {
		memory.OAuthToken = tok.b
	}
	f.store.SetProviderRuntimeConfig(memory.ID, memory)
	writeOAuthMatrixDisk(t, f.path, oauthMatrixDisk(c, tok))
	if c.config == "corrupt" {
		require.NoError(t, os.WriteFile(f.path, []byte("{corrupt-config"), 0o600))
	}
	journal := oauthJournalPath(f.path, f.used.ID)
	record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(tok.a), Token: tok.b}
	switch c.journal {
	case "validB", "transientIO", "persistentIO":
		data, err := json.Marshal(record)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(journal, data, 0o600))
	case "invalidquarantineOK", "invalidquarantinefail":
		require.NoError(t, os.WriteFile(journal, []byte("{invalid-wal"), 0o600))
	}
	if c.owned {
		f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, record)
		// Install-before-publication is reachable with live A.
		f.store.publishMu.Lock()
		f.store.recordPendingOAuthLocked(f.used.ID, tok.a, tok.b)
		f.store.publishMu.Unlock()
	}
	beforeConfig, err := os.ReadFile(f.path)
	require.NoError(t, err)
	beforeWAL, _ := os.ReadFile(journal)
	oldRead, oldWrite := oauthJournalReadFile, oauthJournalAtomicWrite
	oldRename, oldCopy := oauthJournalQuarantineRename, oauthJournalQuarantineCopy
	restoreSeams := oauthMatrixSeamRestorer()
	configTestHooks.Lock()
	oldSync := configTestHooks.syncParent
	configTestHooks.Unlock()
	var repaired atomic.Bool
	var reads atomic.Int32
	var writes atomic.Int32
	oauthJournalReadFile = func(path string) ([]byte, error) {
		if path == journal && !repaired.Load() {
			n := reads.Add(1)
			if c.journal == "persistentIO" || (c.journal == "transientIO" && n <= 2) {
				return nil, os.ErrPermission
			}
		}
		return oldRead(path)
	}
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		if path == journal {
			writes.Add(1)
			if c.storage == "failthenrepair" && !repaired.Load() {
				return os.ErrPermission
			}
		}
		return oldWrite(path, data, mode)
	}
	oauthJournalReadWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	oauthJournalRetryWait = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	oauthJournalWriteBudget = 5 * time.Millisecond
	if c.journal == "invalidquarantinefail" {
		oauthJournalQuarantineRename = func(source, target string) error {
			if !repaired.Load() {
				return os.ErrPermission
			}
			return oldRename(source, target)
		}
		oauthJournalQuarantineCopy = func(source, target string) error {
			if !repaired.Load() {
				return os.ErrPermission
			}
			return oldCopy(source, target)
		}
	}
	configTestHooks.Lock()
	configTestHooks.syncParent = func(path string) error {
		if c.config == "Bsyncfail" && !repaired.Load() {
			return os.ErrPermission
		}
		return oldSyncOAuthJournalParent(path, oldSync)
	}
	configTestHooks.Unlock()
	steps, attempts, idle := make(chan struct{}), make(chan struct{}, 1), make(chan struct{}, 1)
	gate := func(ctx context.Context) {
		select {
		case <-steps:
		case <-ctx.Done():
		}
	}
	oauthJournalBeforeWorker = gate
	oauthJournalWorkerWait = func(ctx context.Context, _ time.Duration) error {
		select {
		case attempts <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		gate(ctx)
		return ctx.Err()
	}
	oauthJournalWorkerIdle = func(store *ConfigStore, _ string) {
		if store == f.store {
			select {
			case idle <- struct{}{}:
			default:
			}
		}
	}
	type exchangeCall struct {
		token  string
		client *http.Client
	}
	calls := make(chan exchangeCall, 4)
	client := &http.Client{}
	codexRefreshTokenFn = func(ctx context.Context, actual *http.Client, refresh string) (*oauth.Token, error) {
		select {
		case calls <- exchangeCall{refresh, actual}:
			return tok.d, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	workerWatchdog := max(configWriteLockTimeout, codexRefreshLockWait) + 5*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result, callerDone := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		oauthMatrixCleanup(t, f.store, callerDone, f.store.stopOAuthJournalWorkers, workerWatchdog, cancel, restoreSeams, t.Fatalf)
	})
	used := f.used
	if c.used == "B" {
		used.OAuthToken = tok.b
	}
	go func() {
		defer close(callerDone)
		result <- f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, used, client)
	}()
	var refreshErr error
	select {
	case refreshErr = <-result:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("I2: refresh exceeded context and watchdog bounds")
	}
	cancel()
	select {
	case <-callerDone:
	case <-time.After(time.Second):
		t.Fatal("I2: caller result preceded a hung goroutine")
	}
	observed := make([]exchangeCall, 0, 1)
	for len(calls) > 0 {
		observed = append(observed, <-calls)
	}
	assert.Len(t, observed, want.calls, "I3: independently required exchange count")
	for _, call := range observed {
		assert.Same(t, client, call.client, "I3: caller-paired client")
		if want.predecessor != nil {
			assert.Equal(t, want.predecessor.RefreshToken, call.token, "I3: authoritative predecessor")
		} else {
			t.Error("I3: exchange forbidden by independent oracle")
		}
	}
	if want.blockedIO {
		assert.Error(t, refreshErr, "I2: journal I/O must be reported")
	} else if want.final != nil {
		assert.NoError(t, refreshErr, "I2: intact owned lineage must not reject refresh or hide unexpected credential CAS")
	}
	pc, _ := f.store.Config().Providers.Get(f.used.ID)
	assert.Equal(t, want.memory, pc.OAuthToken, "I1: independently required caller memory")
	assertOAuthMatrixPreserved(t, c, f, tok, want, journal, beforeWAL)
	if c.config == "corrupt" || c.config == "Bsyncfail" || c.config == "laterC" || c.config == "logout" {
		after, err := os.ReadFile(f.path)
		require.NoError(t, err)
		assert.Equal(t, beforeConfig, after, "I1: caller cannot rewrite uncertain or arbitrated config")
	}
	requireOAuthRound7Released(t, f)
	// Workers stayed gated throughout caller ownership and release checks.
	repaired.Store(true)
	if c.config == "corrupt" {
		writeOAuthMatrixDisk(t, f.path, tok.a)
	}
	f.store.startOAuthJournalWorker(ScopeGlobal, f.used.ID)
	settled := false
	for range 6 {
		select {
		case steps <- struct{}{}:
		case <-time.After(workerWatchdog):
			t.Error("I5: worker did not accept attempt permit")
			return
		}
		select {
		case <-idle:
			settled = true
		case <-attempts:
		case <-time.After(workerWatchdog):
			t.Error("I5: worker attempt exceeded watchdog")
			return
		}
		if settled {
			break
		}
	}
	assert.True(t, settled, "I5: six repaired real-worker attempts must settle")
	actual, err := readOAuthJournalDiskToken(f.path, f.used.ID)
	require.NoError(t, err)
	assert.Equal(t, want.final, actual, "I5: independently newest legitimate config")
	pc, _ = f.store.Config().Providers.Get(f.used.ID)
	if want.converges {
		assert.Equal(t, want.final, pc.OAuthToken, "I5: owned persistence must converge memory with config")
	} else {
		assert.Equal(t, want.finalMemory, pc.OAuthToken, "I5: no owner cannot manufacture memory persistence")
	}
	assert.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID), "I5: outbox drained")
	_, err = os.Stat(journal)
	assert.True(t, os.IsNotExist(err), "I5: active WAL removed")
	assertOAuthMatrixData(t, f.path)
	t.Logf("exchange=%d reads=%d writes=%d worker-settled=%t", len(observed), reads.Load(), writes.Load(), settled)
}

func oauthMatrixBoundedJoin(join func(), watchdog time.Duration) bool {
	joined := make(chan struct{})
	go func() {
		join()
		close(joined)
	}()
	select {
	case <-joined:
		return true
	case <-time.After(watchdog):
		return false
	}
}

func TestOAuthMatrixHarness_BoundedJoinOnce(t *testing.T) {
	blocked, finished := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	joined := oauthMatrixBoundedJoin(func() {
		calls.Add(1)
		<-blocked
		close(finished)
	}, time.Millisecond)
	close(blocked)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("fake join did not finish after release")
	}
	assert.False(t, joined, "blocked join must return at its watchdog")
	assert.Equal(t, int32(1), calls.Load())
}

func assertOAuthMatrixPreserved(t *testing.T, c oauthMatrixCase, f codexRefreshFixture, tok oauthMatrixTokens, want oauthMatrixWant, journal string, before []byte) {
	t.Helper()
	if c.journal == "persistentIO" && !c.owned && want.blockedIO {
		after, err := os.ReadFile(journal)
		require.NoError(t, err)
		assert.Equal(t, before, after, "I1: unreadable WAL must remain byte-identical")
	}
	if !c.owned && c.journal != "validB" && c.journal != "transientIO" && c.journal != "persistentIO" {
		return
	}
	// Only declared arbitration or a required successor discharges B ownership.
	if c.config == "logout" || c.config == "laterC" && tok.c.ExpiresAt >= tok.b.ExpiresAt {
		return
	}
	preserve := tok.b
	if want.calls == 1 {
		preserve = tok.d
	}
	pc, _ := f.store.Config().Providers.Get(f.used.ID)
	if reflect.DeepEqual(pc.OAuthToken, preserve) {
		return
	}
	disk, diskErr := readOAuthJournalDiskToken(f.path, f.used.ID)
	if diskErr == nil && reflect.DeepEqual(disk, preserve) {
		return
	}
	entry := f.store.oauthOutboxCurrent(f.path, f.used.ID)
	if entry != nil && reflect.DeepEqual(entry.record.Token, preserve) {
		return
	}
	data, err := os.ReadFile(journal)
	var wal oauthJournalRecord
	if err == nil && json.Unmarshal(data, &wal) == nil && reflect.DeepEqual(wal.Token, preserve) {
		return
	}
	t.Error("I1: owned rotation missing from memory, config, outbox and valid WAL")
}

func assertOAuthMatrixData(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(data, &document))
	assert.Equal(t, "preserve-me", document["matrix_owned_data"], "I1: unrelated root data")
	providers, ok := document["providers"].(map[string]any)
	require.True(t, ok)
	pc, ok := providers["openai-codex"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "preserve-provider", pc["matrix_provider_data"], "I1: unrelated provider data")
	assert.Equal(t, map[string]any{"api_key": "fixture-only"}, providers["matrix-unrelated"], "I1: unrelated credentials")
}
