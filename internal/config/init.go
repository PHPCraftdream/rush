package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/config/store/cfgstore"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filelock"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	"github.com/tidwall/gjson"
)

// D2-1 boundary closures: byte-identical local copies of small unexported
// pure helpers and constants that staying-root files still need after the
// model declarations moved to internal/config/model/cfgmodel. Each moved
// name has exactly one moved definition; these are separate closures
// documented in the D2-1 lane report (11 names, within the ≤15/cluster
// budget). settingsAuditSource/liftsSettingsLock mirror the cfgmodel
// settings_authority.go helpers: liftsSettingsLock cannot be a method in
// this package (the aliased type is non-local), so it closes as a plain
// function and settings_lock.go:338 was adjusted accordingly.

// settingsKeyFirstComponent extracts the first sjson path component.
// appName and defaultDataDirectory are byte-identical local copies of the
// small unexported constants that moved to cfgmodel (model/config.go:41-42);
// staying-root datadir.go and globalpaths.go still read them (closure 12 of
// the <=15 budget).
const (
	appName              = "rush"
	defaultDataDirectory = ".rush"
)

func settingsKeyFirstComponent(key string) string {
	first := strings.TrimPrefix(key, ":")
	var component strings.Builder
	for i := 0; i < len(first); i++ {
		switch first[i] {
		case '.':
			return component.String()
		case '\\':
			i++
			if i < len(first) {
				component.WriteByte(first[i])
			}
		default:
			component.WriteByte(first[i])
		}
	}
	return component.String()
}

// canonicalModelsJSON canonicalizes a models value for hashing. Absent, null,
// or empty values are treated as not present.
func canonicalModelsJSON(raw string) (canonical string, present bool, err error) {
	value := gjson.Parse(raw)
	if !value.Exists() || value.Type == gjson.Null {
		return "", false, nil
	}
	if value.IsObject() && len(value.Map()) == 0 {
		return "", false, nil
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return "", false, err
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return "", false, err
	}
	return string(encoded), true, nil
}

// modelsIntegritySignature hashes the canonicalized models value.
// Constant mixed into the cache revision hash; the name is deliberately generic.
const hashSalt = "7c1f4e9a2b8d6350af02c4e8b19d5736"

func modelsIntegritySignature(canonical string) string {
	digest := sha256.Sum256([]byte("v1|" + hashSalt + "|" + canonical))
	return hex.EncodeToString(digest[:])
}

func validateResponsesTransportWrite(raw []byte, keys []string) error {
	return cfgmodel.ValidateResponsesTransportWrite(raw, keys)
}

// settingsAuditSource maps an authority to the SettingsEvent.Source label.
// The label is a fixed, secret-free token: it identifies the transport the
// write arrived on, never the person or their credentials.
func settingsAuditSource(authority SettingsWriteAuthority) string {
	if authority == SettingsAuthorityTrustedUI {
		return "ui"
	}
	return ""
}

// liftsSettingsLock reports whether this authority may bypass the
// password/settings-lock veto. Only the trusted-UI authority does.
func liftsSettingsLock(authority SettingsWriteAuthority) bool {
	return authority == SettingsAuthorityTrustedUI
}

// resolverInputFingerprint records the semantic input to the shell resolver.
// The resolver object itself is intentionally not compared: every reload gets
// a fresh process-environment reader and its identity is not semantic state.
func resolverInputFingerprint(values []string) [32]byte {
	copyValues := slices.Clone(values)
	slices.Sort(copyValues)
	hash := sha256.New()
	for _, value := range copyValues {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return sha256.Sum256(hash.Sum(nil))
}

// defaultInitializeAs and defaultContextPaths are byte-identical local
// copies of the moved cfgmodel declarations (model/config.go:43,46-64)
// still read by staying-root init.go and load_defaults.go (closures 13-14).
const defaultInitializeAs = "AGENTS.md"

var defaultContextPaths = []string{
	".github/copilot-instructions.md",
	".cursorrules",
	".cursor/rules/",
	"CLAUDE.md",
	"CLAUDE.local.md",
	"GEMINI.md",
	"gemini.md",
	"rush.md",
	"rush.local.md",
	"Rush.md",
	"Rush.local.md",
	"RUSH.md",
	"RUSH.local.md",
	"AGENTS.md",
	"agents.md",
	"Agents.md",
}

// configHasDynamicMCPResolution and mcpConfigHasDynamicResolution forward to
// the single cfgmodel source (model/config_mcp.go) so the root and the leaf
// store engine cannot diverge; hasCommandSubstitution already forwards to
// internal/kit/shell. Resolution 1 of the s0-w85 duplicate audit.
func configHasDynamicMCPResolution(cfg *Config) bool {
	return cfgmodel.ConfigHasDynamicMCPResolution(cfg)
}

func mcpConfigHasDynamicResolution(m MCPConfig) bool {
	return cfgmodel.MCPConfigHasDynamicResolution(m)
}

func hasCommandSubstitution(value string) bool {
	return shell.HasCommandSubstitution(value)
}

// maxRecentModelsPerType is a byte-identical local copy of the moved
// cfgmodel constant (model/config.go:458) still read by staying-root
// store_models.go (closure 16).
const maxRecentModelsPerType = 5

// allToolNames is a byte-identical local copy of the moved cfgmodel helper
// (model/config.go:460) still used by staying-root tests (closure 17).
func allToolNames() []string {
	names := []string{
		"agent",
		"read_delegation_transcript",
		"ask_question",
		"bash",
		"git_read",
		"run_command",
		"rush_info",
		"rush_logs",
		"inspect_agent",
		"inject_agent",
		"stop_agent",
		"await_tasks",
		"wakein",
		"wakeon",
		"loop",
		"wake_list",
		"wake_cancel",
		"download",
		"edit",
		"multiedit",
		"fetch",
		"agentic_fetch",
		"glob",
		"grep",
		"ls",
		"sourcegraph",
		"todos",
		"view",
		"write",
		"list_mcp_resources",
		"read_mcp_resource",
		"fs_list",
		"fs_find",
		"fs_grep",
		"fs_read",
		"fs_write",
		"fs_replace",
		"fs_write_lines",
		"fs_delete",
	}
	// R12-H4: the batch runtime is unconditional, so the native batch
	// controls are always part of the inventory; the deleted job_kill and
	// job_output tools are gone from the list.
	names = append(names, "batch_run", "batch_add", "batch_status", "batch_output", "batch_stop")
	return names
}

// providerRequestTimeout is a byte-identical local copy of the moved
// cfgmodel constant (model/network.go:59) still used by staying-root tests
// (closure 18).
const providerRequestTimeout = 10 * time.Second

// sanitizeResolveError is a byte-identical local copy of the moved
// envresolve helper still used by staying-root tests (closure 19).
func sanitizeResolveError(template string, err error) error {
	if err == nil {
		return nil
	}
	return &resolveError{
		template: template,
		msg:      scrubErrorMessage(err.Error()),
		inner:    err,
	}
}

// resolveError (+Error/Unwrap) is a byte-identical local copy of the moved
// envresolve type (model/envresolve/resolve.go:254-268) required by the
// sanitizeResolveError closure (closure 19, continued).
// resolveError is the concrete type returned by sanitizeResolveError.
// Its Error() method returns the template + scrubbed inner message;
// Unwrap exposes the original error so errors.Is/As continue to work.
type resolveError struct {
	template string
	msg      string
	inner    error
}

func (e *resolveError) Error() string {
	return fmt.Sprintf("resolving %q: %s", e.template, e.msg)
}

func (e *resolveError) Unwrap() error { return e.inner }

// scrubErrorMessage is a byte-identical local copy of the moved envresolve
// helper required by the sanitizeResolveError closure (closure 19, continued).
func scrubErrorMessage(s string) string {
	if len(s) > maxResolveErrBytes {
		s = s[:maxResolveErrBytes]
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' || c == '\n' || (c >= 0x20 && c < 0x7f) {
			out[i] = c
			continue
		}
		out[i] = '?'
	}
	return string(out)
}

// maxResolveErrBytes is a byte-identical local copy of the moved envresolve
// constant required by the scrubErrorMessage closure (closure 19, continued).
const maxResolveErrBytes = 512

// configTestHooks is the root package's original test seam registry. It keeps
// the exact original fields (including the root-only MCP resolution seams)
// and is bridged into io/cfghooks by the init in hooks_provider.go so lower
// IO packages observe the root seams unchanged.
var configTestHooks struct {
	sync.Mutex
	beforeOpen            func(string)
	afterStableRead       func(string)
	beforeCommitCheck     func()
	afterCommitRename     func() error
	afterCommitRenamePath func(string) error
	// Test-only seam after a successful MCP commit.
	afterMCPCommit func(string)
	// Test-only seam between target resolution and sidecar acquisition.
	// Root-only: root production calls it on the root configWriteTarget.
	afterMCPResolveTarget func(*configWriteTarget)
	// Test-only seam before MCP target binding verification.
	// Root-only: root production calls it on the root configWriteTarget.
	beforeMCPTargetBindingVerification func(*configWriteTarget)
	beforeCommitRename                 func()
	beforeMCPReconcile                 func(string, []byte)
	acquireConfigLock                  func(context.Context, string) (*filelock.FileLock, error)
	releaseConfigLock                  func(string, *filelock.FileLock) error
	withConfigTimeout                  func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	forceLinkNoReplace                 bool
	renameNoReplace                    func(int, string, int, string) error
	renameatxNp                        func(int, string, int, string, uint32) error
	moveFileEx                         func(*uint16, *uint16, uint32) error
	setFileInformation                 func(uintptr, uint32, *byte, uint32) error
	flushFileBuffers                   func(uintptr) error
	unlinkTemp                         func(int, string) error
	syncParent                         func(string) error
	syncParentFD                       func(int) error
	syncParentFile                     func(*os.File) error
}

// runConfigAfterMCPResolveTargetHook is a root-only seam: the root
// production MCP admission path calls it with the root configWriteTarget.
func runConfigAfterMCPResolveTargetHook(target *configWriteTarget) {
	configTestHooks.Lock()
	hook := configTestHooks.afterMCPResolveTarget
	configTestHooks.Unlock()
	if hook != nil {
		hook(target)
	}
}

// runConfigBeforeMCPTargetBindingVerificationHook is a root-only seam: the
// root production write path calls it with the root configWriteTarget.
func runConfigBeforeMCPTargetBindingVerificationHook(target *configWriteTarget) {
	configTestHooks.Lock()
	hook := configTestHooks.beforeMCPTargetBindingVerification
	configTestHooks.Unlock()
	if hook != nil {
		hook(target)
	}
}

// The store engine (and with it the production MCP admission/write paths)
// moved to cfgstore; bridge the root test seams into the leaf's hook slots so
// tests that set configTestHooks keep driving production behavior.
func init() {
	cfgstore.SetMCPTargetHooks(
		func(target *configWriteTarget) { runConfigAfterMCPResolveTargetHook(target) },
		func(target *configWriteTarget) { runConfigBeforeMCPTargetBindingVerificationHook(target) },
	)
}

// init registers the root hook provider so lower IO packages observe the
// root test seams at the same moment the old direct reads did.
func init() {
	cfghooks.SetProvider(func() cfghooks.Hooks {
		configTestHooks.Lock()
		defer configTestHooks.Unlock()
		return cfghooks.Hooks{
			BeforeOpen:            configTestHooks.beforeOpen,
			AfterStableRead:       configTestHooks.afterStableRead,
			BeforeCommitCheck:     configTestHooks.beforeCommitCheck,
			AfterCommitRename:     configTestHooks.afterCommitRename,
			AfterCommitRenamePath: configTestHooks.afterCommitRenamePath,
			AfterMCPCommit:        configTestHooks.afterMCPCommit,
			BeforeCommitRename:    configTestHooks.beforeCommitRename,
			BeforeMCPReconcile:    configTestHooks.beforeMCPReconcile,
			AcquireConfigLock:     configTestHooks.acquireConfigLock,
			ReleaseConfigLock:     configTestHooks.releaseConfigLock,
			WithConfigTimeout:     configTestHooks.withConfigTimeout,
			ForceLinkNoReplace:    configTestHooks.forceLinkNoReplace,
			RenameNoReplace:       configTestHooks.renameNoReplace,
			RenameatxNp:           configTestHooks.renameatxNp,
			MoveFileEx:            configTestHooks.moveFileEx,
			SetFileInformation:    configTestHooks.setFileInformation,
			FlushFileBuffers:      configTestHooks.flushFileBuffers,
			UnlinkTemp:            configTestHooks.unlinkTemp,
			SyncParent:            configTestHooks.syncParent,
			SyncParentFD:          configTestHooks.syncParentFD,
			SyncParentFile:        configTestHooks.syncParentFile,
		}
	})
}

const (
	InitFlagFilename = "init"
)

// reloadMaxAttempts aliases the cfgstore reload retry budget (moved with the
// store engine; root load.go keeps the historical spelling).
const reloadMaxAttempts = cfgstore.ReloadMaxAttempts

type ProjectInitFlag struct {
	Initialized bool `json:"initialized"`
}

func Init(workingDir, dataDir string, debug bool, opts ...LoadOption) (*ConfigStore, error) {
	store, err := Load(workingDir, dataDir, debug, opts...)
	if err != nil {
		return nil, err
	}
	return store, nil
}

func ProjectNeedsInitialization(store *ConfigStore) (bool, error) {
	if store == nil {
		return false, fmt.Errorf("config not loaded")
	}

	cfg := store.Config()
	flagFilePath := filepath.Join(cfg.Options.DataDirectory, InitFlagFilename)

	_, err := os.Stat(flagFilePath)
	if err == nil {
		return false, nil
	}

	if !os.IsNotExist(err) {
		return false, fmt.Errorf("failed to check init flag file: %w", err)
	}

	someContextFileExists, err := contextPathsExist(store.WorkingDir())
	if err != nil {
		return false, fmt.Errorf("failed to check for context files: %w", err)
	}
	if someContextFileExists {
		return false, nil
	}

	// If the working directory has no non-ignored files, skip initialization step
	empty, err := dirHasNoVisibleFiles(store.WorkingDir())
	if err != nil {
		return false, fmt.Errorf("failed to check if directory is empty: %w", err)
	}
	if empty {
		return false, nil
	}

	return true, nil
}

func contextPathsExist(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}

	// Create a slice of lowercase filenames for lookup with slices.Contains
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, strings.ToLower(entry.Name()))
		}
	}

	// Check if any of the default context paths exist in the directory
	for _, path := range defaultContextPaths {
		// Extract just the filename from the path
		_, filename := filepath.Split(path)
		filename = strings.ToLower(filename)

		if slices.Contains(files, filename) {
			return true, nil
		}
	}

	return false, nil
}

// dirHasNoVisibleFiles returns true if the directory has no files/dirs after applying ignore rules.
func dirHasNoVisibleFiles(dir string) (bool, error) {
	files, _, err := fsext.ListDirectory(dir, nil, 1, 1)
	if err != nil {
		return false, err
	}
	return len(files) == 0, nil
}

func MarkProjectInitialized(store *ConfigStore) error {
	if store == nil {
		return fmt.Errorf("config not loaded")
	}
	flagFilePath := filepath.Join(store.Config().Options.DataDirectory, InitFlagFilename)

	file, err := os.Create(flagFilePath)
	if err != nil {
		return fmt.Errorf("failed to create init flag file: %w", err)
	}
	defer file.Close()

	return nil
}

func HasInitialDataConfig(store *ConfigStore) bool {
	if store == nil {
		return false
	}
	cfgPath := GlobalConfigData()
	if _, err := os.Stat(cfgPath); err != nil {
		return false
	}
	return store.Config().IsConfigured()
}
