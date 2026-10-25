// Ktav sidecar-artifact migration tests: journal conversion success with
// equivalent records and 0600 modes, backup/Ktav conflict protection,
// invalid/quarantined journals left for quarantine, every crash boundary
// with resume, cache mtime preservation, the shared startup deadline,
// secret-free deferred diagnostics, and concurrent converters.
package config

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

const ktavArtifactJournalSecret = "rt-SINGLEUSE-artifact-secret-77"

// ktavArtifactJournalBody is a VALID rotating-provider journal: a rotated,
// possibly single-use refresh token with a full valid record shape.
const ktavArtifactJournalBody = `{"token":{"access_token":"at-artifact","refresh_token":"` + ktavArtifactJournalSecret + `","expires_at":1893456000}}`

func canonicalArtifactTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func ktavArtifactTestEnv(t *testing.T) {
	t.Helper()
	_, dataDir := isolateAllGlobalConfigPaths(t)
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RUSH_DISABLE_DEFAULT_PROVIDERS", "true")
	resetBothFormatsWarnings()
}

func seedKtavArtifactJournal(t *testing.T, dir string) (slot, legacy string) {
	t.Helper()
	slot = filepath.Join(dir, "rush.json")
	legacy = oauthJournalLegacyPath(slot, "openai-codex")
	writeTestFile(t, legacy, ktavArtifactJournalBody)
	return slot, legacy
}

func assertKtavArtifactJournalMigrated(t *testing.T, dir, slot string) {
	t.Helper()
	legacy := oauthJournalLegacyPath(slot, "openai-codex")
	ktav := oauthJournalKtavPath(slot, "openai-codex")
	backup := backupPathFor(legacy)
	require.NoFileExists(t, legacy)
	require.FileExists(t, ktav)
	require.FileExists(t, backup)
	require.NoFileExists(t, migrationRecordPathFor(legacy))

	backupBytes, err := os.ReadFile(backup)
	require.NoError(t, err)
	require.Equal(t, ktavArtifactJournalBody, string(backupBytes), "the backup must hold the original journal bytes")

	tree, err := ktavcodec.ParseJSONLossless(backupBytes)
	require.NoError(t, err)
	raw, err := os.ReadFile(ktav)
	require.NoError(t, err)
	ktavTree, err := ktavcodec.DecodeKtav(raw)
	require.NoError(t, err)
	require.NoError(t, ktavcodec.Equivalent(tree, ktavTree), "the Ktav journal must decode to the identical record")

	info, err := os.Stat(ktav)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, fs.FileMode(0o600), info.Mode().Perm(), "journal mode must stay 0600")
	} else {
		require.Zero(t, info.Mode()&os.ModeType, "journal must stay a regular file")
	}
}

func TestKtavArtifactJournalMigrationSuccess(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	slot, legacy := seedKtavArtifactJournal(t, dir)

	require.NoError(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir))
	assertKtavArtifactJournalMigrated(t, dir, slot)

	// Migration is idempotent once complete.
	require.NoError(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir))
	assertKtavArtifactJournalMigrated(t, dir, slot)

	// The format-aware journal selector now resolves the Ktav spelling.
	require.Equal(t, oauthJournalKtavPath(slot, "openai-codex"), selectOAuthJournalPath(slot, "openai-codex", false))
	require.NoFileExists(t, legacy)
}

func TestKtavArtifactJournalBackupConflictLeavesJSONActive(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	slot, legacy := seedKtavArtifactJournal(t, dir)
	original, err := os.ReadFile(legacy)
	require.NoError(t, err)
	backup := backupPathFor(legacy)
	precious := `{"token":{"access_token":"PRECIOUS"}}`
	writeTestFile(t, backup, precious)

	migrationErr := ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir)
	require.ErrorIs(t, migrationErr, errKtavMigrationConflict)

	onDisk, readErr := os.ReadFile(backup)
	require.NoError(t, readErr)
	require.Equal(t, precious, string(onDisk), "a pre-existing backup must never be rewritten")
	jsonOnDisk, readErr := os.ReadFile(legacy)
	require.NoError(t, readErr)
	require.Equal(t, original, jsonOnDisk, "the JSON journal must stay active and untouched")
	require.NoFileExists(t, oauthJournalKtavPath(slot, "openai-codex"))
}

func TestKtavArtifactJournalInvalidOrQuarantinedUntouched(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	slot := filepath.Join(dir, "rush.json")

	t.Run("malformed json", func(t *testing.T) {
		legacy := oauthJournalLegacyPath(slot, "openai-codex")
		writeTestFile(t, legacy, `{"token":`)
		require.NoError(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir))
		require.FileExists(t, legacy, "an invalid journal must be left for quarantine")
		require.NoFileExists(t, oauthJournalKtavPath(slot, "openai-codex"))
		require.NoFileExists(t, backupPathFor(legacy))
	})

	t.Run("structurally invalid record", func(t *testing.T) {
		legacy := oauthJournalLegacyPath(slot, "openai-codex")
		writeTestFile(t, legacy, `{"token":{"access_token":"","refresh_token":"","expires_at":0}}`)
		require.NoError(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir))
		require.FileExists(t, legacy)
		require.NoFileExists(t, oauthJournalKtavPath(slot, "openai-codex"))
		require.NoFileExists(t, backupPathFor(legacy))
	})

	t.Run("quarantined journal is gone and stays gone", func(t *testing.T) {
		legacy := seedKtavArtifactJournal2(t, slot)
		quarantineOAuthJournal(legacy)
		require.NoFileExists(t, legacy)
		require.NoError(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir))
		require.NoFileExists(t, oauthJournalKtavPath(slot, "openai-codex"))
		require.NoFileExists(t, backupPathFor(legacy))
	})
}

func seedKtavArtifactJournal2(t *testing.T, slot string) string {
	t.Helper()
	legacy := oauthJournalLegacyPath(slot, "openai-codex")
	writeTestFile(t, legacy, ktavArtifactJournalBody)
	return legacy
}

func TestKtavArtifactCrashBoundariesRecovers(t *testing.T) {
	type boundaryCase struct {
		name       string
		step       string
		expectJSON bool
		expectKtav bool
		expectBak  bool
	}
	cases := []boundaryCase{
		{name: "after lock", step: "after-lock", expectJSON: true},
		{name: "before convert", step: "before-convert", expectJSON: true},
		{name: "after convert", step: "after-convert", expectJSON: true},
		{name: "before staging", step: "before-staging", expectJSON: true},
		{name: "during staging", step: "during-staging", expectJSON: true},
		{name: "after record", step: "after-record", expectJSON: true},
		{name: "after ktav rename", step: "after-ktav-rename", expectJSON: true, expectKtav: true},
		{name: "after publish", step: "after-publish", expectJSON: true, expectKtav: true},
		{name: "before archive", step: "before-archive", expectJSON: true, expectKtav: true},
		{name: "after archive", step: "after-archive", expectKtav: true, expectBak: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ktavArtifactTestEnv(t)
			dir := canonicalArtifactTempDir(t)
			slot, legacy := seedKtavArtifactJournal(t, dir)

			previousHook := ktavArtifactMigrationStepHook
			ktavArtifactMigrationStepHook = func(step string) error {
				if step == tc.step {
					return errKtavMigrationCrashInjected
				}
				return nil
			}
			migrationErr := ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir)
			ktavArtifactMigrationStepHook = previousHook
			require.ErrorIs(t, migrationErr, errKtavMigrationCrashInjected)

			require.Equal(t, tc.expectJSON, fileExists(legacy))
			require.Equal(t, tc.expectKtav, fileExists(oauthJournalKtavPath(slot, "openai-codex")))
			require.Equal(t, tc.expectBak, fileExists(backupPathFor(legacy)))
			// Byte-safety invariant: the JSON is intact and active, or the
			// Ktav and the backup both exist. Never anything else.
			if !tc.expectJSON {
				require.True(t, tc.expectKtav && tc.expectBak, "no JSON without a verified Ktav and backup")
			}

			// Recovery converges without losing bytes.
			require.NoError(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir))
			assertKtavArtifactJournalMigrated(t, dir, slot)
		})
	}
}

func TestKtavArtifactCacheMTimePreserved(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	providers := cachePathFor("providers")
	writeTestFile(t, providers, `{"providers":[{"name":"p"}]}`)
	old := time.Now().Add(-72 * time.Hour)
	require.NoError(t, os.Chtimes(providers, old, old))
	sourceAge, ageErr := newCache[map[string]any](providers).Age()
	require.NoError(t, ageErr)

	require.NoError(t, migrateKtavArtifact(context.Background(), providers, dir))

	ktav := ktavSiblingOf(providers)
	backup := backupPathFor(providers)
	require.NoFileExists(t, providers)
	require.FileExists(t, ktav)
	require.FileExists(t, backup)
	backupBytes, err := os.ReadFile(backup)
	require.NoError(t, err)
	require.Equal(t, `{"providers":[{"name":"p"}]}`, string(backupBytes))

	info, err := os.Stat(ktav)
	require.NoError(t, err)
	require.WithinDuration(t, old, info.ModTime(), 2*time.Second, "the Ktav cache must inherit the source mtime")

	convertedAge, ageErr := newCache[map[string]any](providers).Age()
	require.NoError(t, ageErr)
	require.WithinDuration(t, old, time.Now().Add(-convertedAge), 2*time.Second, "Age() must be unchanged by conversion")
	require.WithinDuration(t, old, time.Now().Add(-sourceAge), 2*time.Second)
}

func TestKtavArtifactCatalogFetchedAtUntouched(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	catalog := cachePathFor("model-catalog-openai-codex")
	body := `{"fingerprint":"fp-1","fetched_at":"2020-01-01T00:00:00Z","models":[{"id":"m"}]}`
	writeTestFile(t, catalog, body)

	require.NoError(t, migrateKtavArtifact(context.Background(), catalog, dir))

	raw, err := os.ReadFile(ktavSiblingOf(catalog))
	require.NoError(t, err)
	jsonBytes := mustDecodeTestKtav(t, raw)
	require.Contains(t, string(jsonBytes), `"fetched_at":"2020-01-01T00:00:00Z"`, "in-document fetched_at must stay untouched")
}

// TestKtavArtifactSharedStartupDeadline proves journal and cache conversion
// run under ONE shared startup deadline inside Load, and that a secret
// inside the journal bytes never reaches diagnostics.
func TestKtavArtifactSharedStartupDeadline(t *testing.T) {
	ktavSetupLoadMigrationTest(t, true)
	resetBothFormatsWarnings()
	savedBudget := ktavMigrationBudget
	ktavMigrationBudget = 500 * time.Millisecond
	t.Cleanup(func() { ktavMigrationBudget = savedBudget })

	dir := canonicalArtifactTempDir(t)
	projectDataDir := filepath.Join(dir, "workspace-data")
	require.NoError(t, os.MkdirAll(projectDataDir, 0o700))
	writeTestFile(t, filepath.Join(dir, "rush.json"), `{"options":{"debug":true},"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}}}`)
	writeTestFile(t, filepath.Join(projectDataDir, "rush.json"), `{"options":{"debug":true}}`)

	globalDataSlot := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalDataSlot), 0o700))
	journal := oauthJournalLegacyPath(globalDataSlot, "openai-codex")
	writeTestFile(t, journal, ktavArtifactJournalBody)
	providersCache := cachePathFor("providers")
	require.NoError(t, os.MkdirAll(filepath.Dir(providersCache), 0o700))
	writeTestFile(t, providersCache, `{"providers":[]}`)

	journalLock := oauthRefreshLockPath(globalDataSlot, "openai-codex")
	cacheLock := providersCache + ".lock"
	var deadlinesMu sync.Mutex
	var deadlines []time.Time
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if normalizeReloadPath(path) == normalizeReloadPath(journalLock) || normalizeReloadPath(path) == normalizeReloadPath(cacheLock) {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			deadlinesMu.Lock()
			deadlines = append(deadlines, deadline)
			deadlinesMu.Unlock()
			// Block briefly so the conversion records a lock-timeout deferred
			// warning WITHOUT exhausting the shared deadline for later steps.
			blockCtx, blockCancel := context.WithTimeout(ctx, 50*time.Millisecond)
			<-blockCtx.Done()
			blockCancel()
			return nil, blockCtx.Err()
		}
		return filelock.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
	})

	logs := captureSlogOutput(t)
	started := time.Now()
	store, err := Load(dir, projectDataDir, false)
	elapsed := time.Since(started)
	require.NoError(t, err)
	require.NotNil(t, store)
	t.Cleanup(func() { store.stopOAuthJournalWorkers() })

	deadlinesMu.Lock()
	captured := append([]time.Time(nil), deadlines...)
	deadlinesMu.Unlock()
	require.GreaterOrEqual(t, len(captured), 2, "journal and cache conversion should both be attempted")
	require.Equal(t, captured[0], captured[1], "all artifact conversions share Load's single deadline")
	require.Less(t, elapsed, 2*time.Second, "no fresh per-file timeout may be added")

	require.Contains(t, logs.String(), "deferred")
	require.NotContains(t, logs.String(), ktavArtifactJournalSecret, "journal bytes must never reach diagnostics")
	// Deferred, not destructive: the JSON stays active.
	require.FileExists(t, journal)
	require.NoFileExists(t, oauthJournalKtavPath(globalDataSlot, "openai-codex"))
	require.FileExists(t, providersCache)
	require.NoFileExists(t, ktavSiblingOf(providersCache))
}

func TestKtavArtifactDeferredWarningOnLockFailure(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	slot, legacy := seedKtavArtifactJournal(t, dir)

	savedGate := ktavAutoMigrate
	ktavAutoMigrate = true
	t.Cleanup(func() { ktavAutoMigrate = savedGate })
	resetBothFormatsWarnings()

	lock, err := acquireConfigFileLock(context.Background(), oauthRefreshLockPath(slot, "openai-codex"))
	require.NoError(t, err)
	defer func() { _ = releaseConfigFileLock(oauthRefreshLockPath(slot, "openai-codex"), lock) }()

	logs := captureSlogOutput(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	ktavMigrateOAuthJournalsAtStartup(ctx, []string{slot}, dir)

	require.Contains(t, logs.String(), "deferred")
	require.Contains(t, logs.String(), legacy, "the warning carries paths only")
	require.NotContains(t, logs.String(), ktavArtifactJournalSecret)
	require.FileExists(t, legacy, "the JSON journal stays active")
	require.NoFileExists(t, oauthJournalKtavPath(slot, "openai-codex"))
}

func TestKtavArtifactConcurrentConverters(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	slot, legacy := seedKtavArtifactJournal(t, dir)

	const converters = 2
	done := make(chan error, converters)
	for range converters {
		go func() { done <- ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir) }()
	}
	for range converters {
		require.NoError(t, <-done)
	}
	assertKtavArtifactJournalMigrated(t, dir, slot)
}
