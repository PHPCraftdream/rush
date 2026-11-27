package config

// Root-side aliases for store-engine members that moved to
// internal/config/store/cfgstore in the s0-w85-cfgstore move. The white-box
// tests of the moved code stay in this package (they use config.Load and the
// root test hooks and therefore cannot move without an import cycle), so
// this file restores the historical unqualified names:
//   - type/var/const aliases preserve identity (sentinel errors, seam vars);
//   - seam vars are aliased as POINTERS into the leaf so test swaps affect
//     the production call sites (call sites use (*name)(...) spelling);
//   - function aliases forward to cfgstore wrapper exports.
// The newTestConfigStore/testStoreOpts shim forwards to
// cfgstore.NewTestConfigStore. configSyncIsolate/writeSignedModelsFixture are
// verbatim root copies of the helpers that moved with
// store_config_disk_sync_test.go.

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/store/cfgstore"
	"github.com/PHPCraftdream/rush/internal/config/store/mcpstore"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthcred"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
	"github.com/stretchr/testify/require"
)

// Type aliases (moved types).
type (
	mcpJSONEdit             = mcpstore.JSONEdit
	mcpLockedFiles          = mcpstore.LockedFiles
	mcpAdmissionToken       = mcpstore.AdmissionToken
	mcpEvaluation           = mcpstore.Evaluation
	mcpFileRecord           = mcpstore.FileRecord
	mcpAdmissionReadSeeker  = mcpstore.AdmissionReadSeeker
	mcpJSONMember           = mcpstore.JSONMember
	mcpJSONObject           = mcpstore.JSONObject
	oauthDiskSyncState      = oauthcred.DiskSyncState
	oauthJournalRecord      = docread.OAuthJournalRecord
	oauthOutboxEntry        = oauthjournal.Entry
	oauthProviderDescriptor = oauthcred.Descriptor
)

// Sentinel-error aliases (identity preserved).
var (
	errOAuthJournalDisk    = oauthjournal.ErrOAuthJournalDisk
	errOAuthJournalInvalid = oauthjournal.ErrOAuthJournalInvalid
	errOAuthJournalRead    = oauthjournal.ErrOAuthJournalRead

	errKtavWriteNotSupported         = cfgstore.ErrKtavWriteNotSupported
	errReloadProviderEndpointDropped = cfgstore.ErrReloadProviderEndpointDropped
)

// Constant aliases.
const (
	configWriteLockTimeout         = cfgstore.ConfigWriteLockTimeout
	internalConfigWriteLockTimeout = cfgstore.InternalConfigWriteLockTimeout
	mcpAdmissionVerifyBufferSize   = mcpstore.MCPAdmissionVerifyBufferSize
)

// Seam pointer aliases: tests swap the pointed-to production values.
var (
	codexRefreshTokenFn              = cfgstore.CodexRefreshTokenFn
	copilotRefreshTokenFn            = cfgstore.CopilotRefreshTokenFn
	hyperExchangeTokenFn             = cfgstore.HyperExchangeTokenFn
	oauthProviderDescriptorsOverride = cfgstore.OauthProviderDescriptorsOverr
	configDiskSyncWait               = cfgstore.ConfigDiskSyncWait
	configDiskSyncRandInt63          = cfgstore.ConfigDiskSyncRandInt63
	configDiskSyncFirstDelay         = cfgstore.ConfigDiskSyncFirstDelay
	configDiskSyncInterval           = cfgstore.ConfigDiskSyncInterval
	codexRefreshLockWait             = cfgstore.CodexRefreshLockWait
	configDiskSyncAfterTick          = cfgstore.ConfigDiskSyncAfterTick

	oauthJournalWorkerDone    = cfgstore.OauthJournalWorkerDone
	oauthJournalWorkerIdle    = cfgstore.OauthJournalWorkerIdle
	codexRefreshBeforePersist = cfgstore.CodexRefreshBeforePersist
)

// Function-value aliases forwarding to the owning lower packages.
var (
	arbitrateOAuthJournal         = oauthjournal.Arbitrate
	oauthJournalPath              = oauthjournal.JournalPath
	oauthRefreshLockPath          = oauthjournal.RefreshLockPath
	oauthRefreshTokenSHA256       = oauthjournal.RefreshTokenSHA256
	oauthRotationDigest           = oauthjournal.RotationDigest
	oauthJournalPredecessor       = oauthjournal.OAuthJournalPredecessor
	oauthJournalDiskLaterOrTie    = oauthjournal.OAuthJournalDiskLaterOrTie
	oauthJournalLater             = oauthjournal.OAuthJournalLater
	oauthJournalLivePredecessor   = oauthjournal.LivePredecessor
	oauthJournalWithBurned        = oauthjournal.WithBurned
	pruneOAuthJournalQuarantines  = oauthjournal.PruneQuarantines
	quarantineOAuthJournal        = oauthjournal.Quarantine
	readOAuthJournal              = oauthjournal.ReadJournal
	readOAuthJournalDiskToken     = oauthjournal.ReadJournalDiskToken
	readOAuthJournalWithRetry     = oauthjournal.ReadJournalWithRetry
	writeOAuthJournalOnce         = oauthjournal.WriteJournalOnce
	writeOAuthJournalUntilDurable = oauthjournal.WriteJournalUntilDurable
)

// Function aliases forwarding to cfgstore wrapper exports.
var (
	applyMCPJSONEdits               = cfgstore.ApplyMCPJSONEdits
	cloneOptions                    = cfgstore.CloneOptions
	cloneTUIOptions                 = cfgstore.CloneTUIOptions
	configDiskSyncDelay             = cfgstore.ConfigDiskSyncDelay
	defaultConfigDiskSyncFirstDelay = cfgstore.DefaultConfigDiskSyncFirstDelay
	configMCPAdmissionLockGroups    = cfgstore.ConfigMCPAdmissionLockGroups
	dataFingerprint                 = cfgstore.DataFingerprint
	editMCPDocument                 = cfgstore.EditMCPDocument
	entryOrigins                    = cfgstore.EntryOrigins
	lastMCPMember                   = cfgstore.LastMCPMember
	mcpCommitWasReconciled          = cfgstore.MCPCommitWasReconciled
	mcpContainerName                = cfgstore.MCPContainerName
	oauthProviderDescriptors        = cfgstore.OAuthProviderDescriptors
	rejectUnconvertedKtavWrite      = cfgstore.RejectUnconvertedKtavWrite
	scanMCPJSONObject               = cfgstore.ScanMCPJSONObject
	scanMCPJSONValue                = cfgstore.ScanMCPJSONValue
	updateMCPFile                   = cfgstore.UpdateMCPFile
	validateFinalMCPAdmissionFile   = cfgstore.ValidateFinalMCPAdmissionFile
	validatePreparedAdmissionFile   = cfgstore.ValidatePreparedAdmissionFile
	verifyMCPAdmissionBytes         = cfgstore.VerifyMCPAdmissionBytes
	verifyConfigTarget              = cfgstore.VerifyConfigTarget
	verifyConfigTargetBinding       = cfgstore.VerifyConfigTargetBinding
	waitCodexRefresh                = cfgstore.WaitCodexRefresh
)

// testStoreOpts/newTestConfigStore shim forwarding to the leaf constructor.
type testStoreOpts struct {
	config         *Config
	globalDataPath string
	workspacePath  string
	resolver       VariableResolver
	loadedPaths    []string
}

func newTestConfigStore(opts testStoreOpts) *ConfigStore {
	return cfgstore.NewTestConfigStore(cfgstore.TestStoreOpts{
		Config:         opts.config,
		GlobalDataPath: opts.globalDataPath,
		WorkspacePath:  opts.workspacePath,
		Resolver:       opts.resolver,
		LoadedPaths:    opts.loadedPaths,
	})
}

// configSyncIsolate is the verbatim root copy of the helper that moved with
// store_config_disk_sync_test.go into the cfgstore package.
func configSyncIsolate(t *testing.T) (dir, configPath, globalPath string) {
	t.Helper()
	dir = t.TempDir()
	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)
	configPath = filepath.Join(dir, "rush.json")
	globalPath = GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
	return dir, configPath, globalPath
}

// writeSignedModelsFixture is the verbatim root copy of the helper that
// moved with store_config_disk_sync_test.go into the cfgstore package.
func writeSignedModelsFixture(t *testing.T, path, modelsJSON string) {
	t.Helper()
	doc := `{"options":{"disable_default_providers":true},"models":` + modelsJSON +
		`,"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`
	signed, err := applyModelsIntegrity(doc)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(signed), 0o600))
}

// syncBuffer is the verbatim root copy of the log-buffer helper that moved
// with store_config_disk_sync_test.go into the cfgstore package.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
