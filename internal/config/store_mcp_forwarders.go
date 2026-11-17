package config

// X2 mcpstore forwarders: rule-18 identity aliases for the declarations that
// moved to internal/config/store/mcpstore, plus root-side forwarders for the
// unexported names the staying-root white-box tests (config_regression_test,
// config_sidecar_identity_test, store_mcp_test, store_mcp_format_test,
// mcp_admission_io_test, store_mcp_ktav_test, store_mcp_identity_unix_test,
// docker_mcp_test) and the root write path still call by their original
// names. Each forwarded name keeps exactly one definition (in mcpstore).

import (
	"context"
	"os"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	"github.com/PHPCraftdream/rush/internal/config/store/mcpstore"
)

// Moved exported types/errors (single identity, re-exported as aliases).
type (
	MCPAdmissionGuard    = mcpstore.MCPAdmissionGuard
	MCPAdmissionSnapshot = mcpstore.MCPAdmissionSnapshot
	MCPMutationResult    = mcpstore.MCPMutationResult

	// White-box test aliases: the staying-root tests keep the original
	// unexported type names for the mcpstore types they construct.
	mcpAdmissionToken      = mcpstore.AdmissionToken
	mcpEvaluation          = mcpstore.Evaluation
	mcpFileRecord          = mcpstore.FileRecord
	mcpAdmissionReadSeeker = mcpstore.AdmissionReadSeeker
	mcpJSONEdit            = mcpstore.JSONEdit
	mcpJSONMember          = mcpstore.JSONMember
	mcpJSONObject          = mcpstore.JSONObject
	mcpLockedFiles         = mcpstore.LockedFiles
)

var (
	ErrMCPNotFound         = mcpstore.ErrMCPNotFound
	ErrMCPExternal         = mcpstore.ErrMCPExternal
	ErrMCPAmbiguous        = mcpstore.ErrMCPAmbiguous
	ErrMCPUnwritableOrigin = mcpstore.ErrMCPUnwritableOrigin
	ErrMCPTargetExists     = mcpstore.ErrMCPTargetExists
	ErrMCPStale            = mcpstore.ErrMCPStale
	ErrMCPCommitUncertain  = mcpstore.ErrMCPCommitUncertain
	ErrMCPMutationStale    = mcpstore.ErrMCPMutationStale

	// ErrMCPHardLinkTopology moved with the locked-file set (mcpstore).
	ErrMCPHardLinkTopology = mcpstore.ErrMCPHardLinkTopology
)

// Store method forwarders (original unexported names, mcpstore bodies).
func (s *ConfigStore) evaluateMCPFiles(files *mcpLockedFiles) (mcpEvaluation, error) {
	return s.mcpStore().EvaluateMCPFiles(files)
}

func (s *ConfigStore) withMCPAdmissionLocks(fn func(*mcpLockedFiles) error) error {
	return s.mcpStore().WithMCPAdmissionLocks(fn)
}

func (s *ConfigStore) withMCPLocksUsingCaseSensitivity(
	ctx context.Context,
	fn func(*mcpLockedFiles) error,
	caseSensitivity func(string) casefold.ConfigCaseSensitivity,
) error {
	return s.mcpStore().WithMCPLocksUsingCaseSensitivity(ctx, fn, caseSensitivity)
}

func (s *ConfigStore) applyCommittedMCPFingerprint(files *mcpLockedFiles, record *mcpFileRecord, committed docread.Fingerprint) error {
	return s.mcpStore().ApplyCommittedMCPFingerprint(files, record, committed)
}

func (s *ConfigStore) prepareMCPFileMutation(files *mcpLockedFiles, path, operation, oldName, newName string, value MCPConfig, disabled *bool) error {
	return s.mcpStore().PrepareMCPFileMutation(files, path, operation, oldName, newName, value, disabled)
}

func (s *ConfigStore) mcpPathData(files *mcpLockedFiles, path string) ([]byte, bool, error) {
	return s.mcpStore().MCPPathData(files, path)
}

func (s *ConfigStore) orderedMCPPaths() []string { return s.mcpStore().OrderedMCPPaths() }

// Package-level forwarders.
func mcpCommitWasReconciled(err error) bool { return mcpstore.MCPCommitWasReconciled(err) }

func editMCPDocument(data []byte, path, operation, oldName, newName string, value MCPConfig, disabled *bool, mutate func(map[string]any) error) ([]byte, error) {
	return mcpstore.EditMCPDocument(data, path, operation, oldName, newName, value, disabled, mutate)
}

func updateMCPFile(files *mcpLockedFiles, path, name string, mutate func(map[string]any) error) error {
	return mcpstore.UpdateMCPFile(files, path, name, mutate)
}

func applyMCPJSONEdits(data []byte, edits ...mcpJSONEdit) []byte {
	return mcpstore.ApplyMCPJSONEdits(data, edits...)
}

func scanMCPJSONObject(data []byte) (mcpJSONObject, error) {
	return mcpstore.ScanMCPJSONObject(data)
}

func scanMCPJSONValue(data []byte, offset int) (int, *mcpJSONObject, error) {
	return mcpstore.ScanMCPJSONValue(data, offset)
}

func lastMCPMember(members []mcpJSONMember, key string) *mcpJSONMember {
	return mcpstore.LastMCPMember(members, key)
}

func mcpContainerName(path string) string { return mcpstore.MCPContainerName(path) }

func entryOrigins(origins map[string]MCPOrigin, path, workspacePath, globalPath, systemPath string, data []byte) {
	mcpstore.EntryOrigins(origins, path, workspacePath, globalPath, systemPath, data)
}

func (s *ConfigStore) configPathOrEmpty(scope Scope) string {
	path, _ := s.configPath(scope)
	return path
}

func configMCPAdmissionLockGroups(targets []configWriteTarget, caseSensitivity func(string) casefold.ConfigCaseSensitivity) ([]*mcpstore.LockGroup, map[string]mcpstore.LockTarget, map[string]string) {
	lockTargets := make([]mcpstore.LockTarget, 0, len(targets))
	for _, target := range targets {
		lockTargets = append(lockTargets, mcpstore.LockTarget{Path: target.path, SelectedPath: target.selectedPath, LockPath: target.lockPath, Expected: target.expected, Owner: target.owner, Enforce: target.enforce})
	}
	return mcpstore.ConfigMCPAdmissionLockGroups(lockTargets, caseSensitivity)
}

const mcpAdmissionVerifyBufferSize = mcpstore.MCPAdmissionVerifyBufferSize

func validateFinalMCPAdmissionFile(ctx context.Context, path string, file *os.File, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	return mcpstore.ValidateFinalMCPAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner)
}

func verifyMCPAdmissionBytes(ctx context.Context, file mcpAdmissionReadSeeker, expectedSize int64, expectedDigest [32]byte) error {
	return mcpstore.VerifyMCPAdmissionBytes(ctx, file, expectedSize, expectedDigest)
}

// Docker MCP package-level functions moved to mcpstore; the root re-exports
// them (no remaining external callers outside docker_mcp_test.go today).
func IsDockerMCPAvailable() bool { return mcpstore.IsDockerMCPAvailable() }

func DockerMCPAvailabilityCached() (bool, bool) { return mcpstore.DockerMCPAvailabilityCached() }

func RefreshDockerMCPAvailability() bool { return mcpstore.RefreshDockerMCPAvailability() }

func DockerMCPConfig() MCPConfig { return mcpstore.DockerMCPConfig() }

// dataFingerprint moved to mcpstore (DataFingerprint); the root write path
// (store_write.go noteInitialLoadWriteLocked) keeps the original name.
func dataFingerprint(path string, data []byte) docread.Fingerprint {
	return mcpstore.DataFingerprint(path, data)
}

func validatePreparedAdmissionFile(ctx context.Context, path string, file *os.File, expected docread.Fingerprint, expectedOwner int, enforceOwner bool) error {
	return mcpstore.ValidatePreparedAdmissionFile(ctx, path, file, expected, expectedOwner, enforceOwner)
}
