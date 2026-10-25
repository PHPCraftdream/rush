// Ktav sidecar-artifact migration tests: journal conversion success with
// equivalent records and 0600 modes, backup/Ktav conflict protection,
// invalid/quarantined journals left for quarantine, every crash boundary
// with resume, cache mtime preservation, the shared startup deadline,
// secret-free deferred diagnostics, and concurrent converters.
package config

import (
	"context"
	"errors"
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

	// The production journal reader resolves the SELECTED path back to the
	// original token: one source of truth for the destination name.
	selected := selectOAuthJournalPath(slot, "openai-codex", false)
	require.Equal(t, oauthJournalKtavPath(slot, "openai-codex"), selected)
	recovered, readErr := readOAuthJournalWithRetry(context.Background(), selected)
	require.NoError(t, readErr)
	require.NotNil(t, recovered)
	require.NotNil(t, recovered.Token)
	require.Equal(t, "at-artifact", recovered.Token.AccessToken)
	require.Equal(t, ktavArtifactJournalSecret, recovered.Token.RefreshToken)
	require.EqualValues(t, 1893456000, recovered.Token.ExpiresAt)

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
		configHook bool
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
		// The archival-internal boundaries fire the SHARED config hook
		// (runKtavMigrationStep inside archiveKtavBackup), not the artifact
		// hook. During-backup-rename stops BEFORE the rename, so the JSON is
		// still intact. After-backup-rename is a post-archive crash: the
		// record survives beside the Ktav and the backup.
		{name: "during backup rename", step: "during-backup-rename", configHook: true, expectJSON: true, expectKtav: true},
		{name: "after backup rename", step: "after-backup-rename", configHook: true, expectKtav: true, expectBak: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ktavArtifactTestEnv(t)
			dir := canonicalArtifactTempDir(t)
			slot, legacy := seedKtavArtifactJournal(t, dir)

			previousArtifactHook := ktavArtifactMigrationStepHook
			previousConfigHook := ktavMigrationStepHook
			crash := func(step string) error {
				if step == tc.step {
					return errKtavMigrationCrashInjected
				}
				return nil
			}
			if tc.configHook {
				ktavMigrationStepHook = crash
			} else {
				ktavArtifactMigrationStepHook = crash
			}
			t.Cleanup(func() {
				ktavArtifactMigrationStepHook = previousArtifactHook
				ktavMigrationStepHook = previousConfigHook
			})
			migrationErr := ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir)
			require.ErrorIs(t, migrationErr, errKtavMigrationCrashInjected)

			require.Equal(t, tc.expectJSON, fileExists(legacy))
			require.Equal(t, tc.expectKtav, fileExists(oauthJournalKtavPath(slot, "openai-codex")))
			require.Equal(t, tc.expectBak, fileExists(backupPathFor(legacy)))
			// Byte-safety invariant, asserted on the OBSERVED files: the JSON is
			// intact and active, or the Ktav and the backup both exist. Never
			// anything else.
			jsonThere := fileExists(legacy)
			ktavThere := fileExists(oauthJournalKtavPath(slot, "openai-codex"))
			bakThere := fileExists(backupPathFor(legacy))
			require.True(t, jsonThere || (ktavThere && bakThere), "no JSON without a verified Ktav and backup")

			// Recovery converges without losing bytes (hooks released first).
			ktavArtifactMigrationStepHook = previousArtifactHook
			ktavMigrationStepHook = previousConfigHook
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
	require.FileExists(t, legacy, "the seed journal must exist before the race")

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

// TestKtavArtifactCacheReaderResolvesConvertedCaches proves the production
// cache reader (readCacheDocument) decodes the CONVERTED Ktav generation back
// to the original JSON document for every Rush-owned cache slot.
func TestKtavArtifactCacheReaderResolvesConvertedCaches(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	cases := []struct{ name, slot, body string }{
		{"providers", "providers", `{"providers":[{"name":"p","api_key":"k"}]}`},
		{"hyper", "hyper", `{"endpoints":[{"url":"https://example.invalid"}]}`},
		{"model catalog", "model-catalog-openai-codex", `{"fingerprint":"fp-1","fetched_at":"2020-01-01T00:00:00Z","models":[{"id":"m"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			legacy := cachePathFor(tc.slot)
			writeTestFile(t, legacy, tc.body)

			require.NoError(t, migrateKtavArtifact(context.Background(), legacy, dir))

			require.NoFileExists(t, legacy)
			require.FileExists(t, ktavSiblingOf(legacy))
			require.FileExists(t, backupPathFor(legacy))
			converted, err := readCacheDocument(legacy)
			require.NoError(t, err)
			require.JSONEq(t, tc.body, string(converted), "the production reader must decode the converted Ktav to the original JSON document")
		})
	}
}

// TestKtavArtifactExistingKtavConflictLeavesKtavActive proves the conflict
// contract: when the destination Ktav already exists, both files are left
// untouched, no backup is created, and the production readers resolve the
// EXISTING Ktav (readers prefer the Ktav generation — the JSON is ignored).
func TestKtavArtifactExistingKtavConflictLeavesKtavActive(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)

	t.Run("journal", func(t *testing.T) {
		slot, legacy := seedKtavArtifactJournal(t, dir)
		dest := oauthJournalKtavPath(slot, "openai-codex")
		existingKtav := testKtavBytes(t, ktavArtifactJournalBody)
		require.NoError(t, os.WriteFile(dest, existingKtav, 0o600))
		original, err := os.ReadFile(legacy)
		require.NoError(t, err)

		require.ErrorIs(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir), errKtavMigrationConflict)

		jsonOnDisk, readErr := os.ReadFile(legacy)
		require.NoError(t, readErr)
		require.Equal(t, string(original), string(jsonOnDisk), "the JSON is never rewritten")
		ktavOnDisk, readErr := os.ReadFile(dest)
		require.NoError(t, readErr)
		require.Equal(t, existingKtav, ktavOnDisk, "the pre-existing Ktav is never replaced")
		require.NoFileExists(t, backupPathFor(legacy))
		require.Equal(t, dest, selectOAuthJournalPath(slot, "openai-codex", false), "the production selector resolves the pre-existing Ktav")
		recovered, readErr := readOAuthJournalWithRetry(context.Background(), selectOAuthJournalPath(slot, "openai-codex", false))
		require.NoError(t, readErr)
		require.NotNil(t, recovered)
		require.NotNil(t, recovered.Token)
		require.Equal(t, ktavArtifactJournalSecret, recovered.Token.RefreshToken, "the active Ktav journal still decodes to the original token")
	})

	t.Run("cache", func(t *testing.T) {
		legacy := cachePathFor("providers")
		writeTestFile(t, legacy, `{"providers":[{"name":"json"}]}`)
		dest := ktavSiblingOf(legacy)
		existingBody := `{"providers":[{"name":"existing-ktav"}]}`
		writeTestFile(t, dest, string(testKtavBytes(t, existingBody)))

		require.ErrorIs(t, migrateKtavArtifact(context.Background(), legacy, dir), errKtavMigrationConflict)

		jsonOnDisk, err := os.ReadFile(legacy)
		require.NoError(t, err)
		require.JSONEq(t, `{"providers":[{"name":"json"}]}`, string(jsonOnDisk), "the JSON is never rewritten")
		converted, err := readCacheDocument(legacy)
		require.NoError(t, err)
		require.JSONEq(t, existingBody, string(converted), "the production cache reader resolves the pre-existing Ktav")
		require.NoFileExists(t, backupPathFor(legacy))
	})
}

// TestKtavArtifactChtimesFailureDefersConversion proves mtime preservation is
// a HARD step: an injected Chtimes failure stops the transaction before the
// archival (JSON stays active, NO backup), and a later run with a working
// Chtimes converges to a fully migrated state with the source mtime.
func TestKtavArtifactChtimesFailureDefersConversion(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	providers := cachePathFor("providers")
	writeTestFile(t, providers, `{"providers":[{"name":"p"}]}`)
	old := time.Now().Add(-72 * time.Hour)
	require.NoError(t, os.Chtimes(providers, old, old))

	savedStat, savedChtimes := ktavArtifactStatFile, ktavArtifactChtimes
	ktavArtifactChtimes = func(string, time.Time, time.Time) error {
		return errors.New("injected chtimes failure")
	}
	t.Cleanup(func() {
		ktavArtifactStatFile, ktavArtifactChtimes = savedStat, savedChtimes
	})

	err := migrateKtavArtifact(context.Background(), providers, dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mtime", "the failure names the mtime step")
	require.FileExists(t, providers, "the JSON stays active: the failure happens before archival")
	require.NoFileExists(t, backupPathFor(providers), "no backup is created")
	require.NoFileExists(t, backupPathFor(providers))

	// The staged Ktav plus its record converge on the next run with a
	// working Chtimes.
	require.NoError(t, migrateKtavArtifact(context.Background(), providers, dir))
	require.NoFileExists(t, providers)
	require.FileExists(t, ktavSiblingOf(providers))
	require.FileExists(t, backupPathFor(providers))
	require.NoFileExists(t, migrationRecordPathFor(providers))
	info, statErr := os.Stat(ktavSiblingOf(providers))
	require.NoError(t, statErr)
	require.WithinDuration(t, old, info.ModTime(), 2*time.Second, "the converging run re-applies the source mtime")
}

// TestKtavArtifactJournalInvalidatedUnderLock proves the journal is
// re-validated as an OAuth record UNDER the refresh lock, on the exact bytes
// the converter would convert: a journal replaced with structurally invalid
// JSON while the lock was contended is skipped, not converted or archived.
func TestKtavArtifactJournalInvalidatedUnderLock(t *testing.T) {
	ktavArtifactTestEnv(t)
	dir := canonicalArtifactTempDir(t)
	slot, legacy := seedKtavArtifactJournal(t, dir)
	invalid := `{"token":{"access_token":"","refresh_token":"","expires_at":0}}`

	previousHook := ktavArtifactMigrationStepHook
	ktavArtifactMigrationStepHook = func(step string) error {
		if step == "after-lock" {
			// Syntactically valid, structurally invalid: fire between lock
			// acquisition and conversion.
			writeTestFile(t, legacy, invalid)
		}
		return nil
	}
	t.Cleanup(func() { ktavArtifactMigrationStepHook = previousHook })

	require.NoError(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir), "an invalid journal is skipped, not an error")

	onDisk, err := os.ReadFile(legacy)
	require.NoError(t, err)
	require.Equal(t, invalid, string(onDisk), "the replaced content is left untouched for quarantine")
	require.NoFileExists(t, oauthJournalKtavPath(slot, "openai-codex"))
	require.NoFileExists(t, backupPathFor(legacy))
	require.NoFileExists(t, migrationRecordPathFor(legacy))
}

// TestKtavLoadConvertsCachesRegardlessOfWorkspaceFormat proves the cache
// startup step runs OUTSIDE the workspace-config conditional: an
// already-migrated (rush.ktav) workspace AND a workspace with no config at
// all both convert a present legacy providers.json cache under Load.
func TestKtavLoadConvertsCachesRegardlessOfWorkspaceFormat(t *testing.T) {
	cases := []struct{ name, workspaceFile string }{
		{name: "workspace already rush.ktav", workspaceFile: "rush.ktav"},
		{name: "no workspace config at all", workspaceFile: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ktavSetupLoadMigrationTest(t, true)
			resetBothFormatsWarnings()
			dir := canonicalArtifactTempDir(t)
			if tc.workspaceFile != "" {
				configJSON := `{"options":{"debug":true},"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"test-key","models":[{"id":"model","name":"Model"}]}}}`
				require.NoError(t, os.WriteFile(filepath.Join(dir, tc.workspaceFile), testKtavBytes(t, configJSON), 0o600))
			}
			providers := cachePathFor("providers")
			require.NoError(t, os.MkdirAll(filepath.Dir(providers), 0o700))
			writeTestFile(t, providers, `{"providers":[{"name":"p"}]}`)

			store, err := Load(dir, dir, false)
			require.NoError(t, err)
			require.NotNil(t, store)
			t.Cleanup(func() { store.stopOAuthJournalWorkers() })

			require.FileExists(t, ktavSiblingOf(providers), "the providers cache must be converted after Load")
			require.NoFileExists(t, providers)
			require.FileExists(t, backupPathFor(providers))
			converted, err := readCacheDocument(providers)
			require.NoError(t, err)
			require.JSONEq(t, `{"providers":[{"name":"p"}]}`, string(converted))
		})
	}
}

// TestKtavArtifactStartupJournalRecoversPostArchiveCrash proves the startup
// journal path picks up a leftover POST-ARCHIVE state (Ktav + backup +
// record, JSON already gone) and converges to a record-retired migration.
func TestKtavArtifactStartupJournalRecoversPostArchiveCrash(t *testing.T) {
	ktavArtifactTestEnv(t)
	resetBothFormatsWarnings()
	dir := canonicalArtifactTempDir(t)
	slot, legacy := seedKtavArtifactJournal(t, dir)

	previousHook := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		if step == "after-backup-rename" {
			return errKtavMigrationCrashInjected
		}
		return nil
	}
	t.Cleanup(func() { ktavMigrationStepHook = previousHook })
	require.ErrorIs(t, ktavMigrateOAuthJournal(context.Background(), slot, "openai-codex", dir), errKtavMigrationCrashInjected)

	// The crash-after-archive state: Ktav + backup + record, no JSON.
	require.NoFileExists(t, legacy)
	require.FileExists(t, oauthJournalKtavPath(slot, "openai-codex"))
	require.FileExists(t, backupPathFor(legacy))
	require.FileExists(t, migrationRecordPathFor(legacy))

	// The startup journal function converges without losing bytes.
	ktavMigrateOAuthJournalsAtStartup(context.Background(), []string{slot}, dir)
	require.NoFileExists(t, migrationRecordPathFor(legacy), "the leftover record must be retired")
	assertKtavArtifactJournalMigrated(t, dir, slot)
	require.Equal(t, oauthJournalKtavPath(slot, "openai-codex"), selectOAuthJournalPath(slot, "openai-codex", false))
}
