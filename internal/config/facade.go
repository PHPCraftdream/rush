// Package config facade over extracted domain packages.
//
// This file is the permanent re-export surface reserved by the package
// design (section 2.5): as sub-packages are extracted under
// internal/config/domains/, their exported API is re-declared here so
// the root package and its existing callers keep compiling unchanged.
// One import group per domain; the file grows one block per lane.
//
// Note on the three peak-hours merge helpers
// (StripPeakHoursOverlays, ResolvePeakHoursOverlays,
// ApplyPeakHoursOverlays): their signatures mention the unexported
// type peak.peakHoursOverlay, so they cannot be aliased or forwarded
// from this package; root callers use them via the peak qualifier.
package config

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config/domains/apiremote"
	"github.com/PHPCraftdream/rush/internal/config/domains/apitoken"
	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/domains/listeners"
	"github.com/PHPCraftdream/rush/internal/config/domains/peak"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"
	"github.com/PHPCraftdream/rush/internal/config/io/fsx/fileid"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	envresolve "github.com/PHPCraftdream/rush/internal/config/model/envresolve"
	cfgmerge "github.com/PHPCraftdream/rush/internal/config/model/merge"
	"github.com/PHPCraftdream/rush/internal/config/providers/catalogsrc"
	"github.com/PHPCraftdream/rush/internal/config/providers/provresolve"
	"github.com/PHPCraftdream/rush/internal/config/store/settings"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
)

// Types re-exported from the peak domain.
type (
	PeakHoursWindow     = peak.PeakHoursWindow
	PeakHoursWindowSpec = peak.PeakHoursWindowSpec
	PeakHoursPolicy     = peak.PeakHoursPolicy
	PeakHoursStatus     = peak.PeakHoursStatus
	ProviderPeakHours   = peak.ProviderPeakHours
)

// Constants re-exported from the peak domain.
const (
	PeakHoursMaxWindows = peak.PeakHoursMaxWindows
	PeakPauseMaxWindow  = peak.PeakPauseMaxWindow
)

// Functions re-exported from the peak domain.
func ParsePeakPauseDuration(spec string, now time.Time) (time.Time, error) {
	return peak.ParsePeakPauseDuration(spec, now)
}

func ParsePeakDays(spec string) ([7]bool, error) {
	return peak.ParsePeakDays(spec)
}

func FormatPeakDays(days [7]bool) string {
	return peak.FormatPeakDays(days)
}

func ParsePeakClock(s string, allowEnd bool) (int, error) {
	return peak.ParsePeakClock(s, allowEnd)
}

func ResolvePeakTimezone(name string) (*time.Location, error) {
	return peak.ResolvePeakTimezone(name)
}

// ---------------------------------------------------------------------------
// D2-1: cfgmodel / cfgmerge / envresolve groups
//
// The config model declarations (config.go, scope.go, config_mcp.go,
// responses_transport.go, finalization_flag.go, load_hooks.go, the custom
// role files, network.go, provider_http.go, settings_authority.go), the
// document merge pipeline (loadFromBytes and the overlay sanitizers from
// load_files.go plus custom_roles_load.go) and the variable resolver
// (resolve.go) moved to internal/config/model/{cfgmodel,cfgmerge,envresolve}
// in D2-1. Everything below re-exports their API so root code and external
// callers keep compiling unchanged.
// ---------------------------------------------------------------------------

// Types re-exported from the cfgmodel package.
type (
	Agent                     = cfgmodel.Agent
	Attribution               = cfgmodel.Attribution
	Completions               = cfgmodel.Completions
	Config                    = cfgmodel.Config
	ContextVariableResolver   = envresolve.ContextVariableResolver
	CustomRole                = cfgmodel.CustomRole
	Expander                  = envresolve.Expander
	FinalizationOptions       = cfgmodel.FinalizationOptions
	HookConfig                = cfgmodel.HookConfig
	MCP                       = cfgmodel.MCP
	MCPConfig                 = cfgmodel.MCPConfig
	MCPOrigin                 = cfgmodel.MCPOrigin
	MCPOriginKind             = cfgmodel.MCPOriginKind
	MCPSource                 = cfgmodel.MCPSource
	MCPType                   = cfgmodel.MCPType
	MCPs                      = cfgmodel.MCPs
	ModelEffortInfo           = cfgmodel.ModelEffortInfo
	ModelPreset               = cfgmodel.ModelPreset
	NetworkConfig             = cfgmodel.NetworkConfig
	Options                   = cfgmodel.Options
	Permissions               = cfgmodel.Permissions
	ProviderConfig            = cfgmodel.ProviderConfig
	ProviderHTTPClientFactory = cfgmodel.ProviderHTTPClientFactory
	ResponsesTransport        = cfgmodel.ResponsesTransport
	RunPermissions            = cfgmodel.RunPermissions
	Scope                     = cfgmodel.Scope
	SelectedModel             = cfgmodel.SelectedModel
	SelectedModelType         = cfgmodel.SelectedModelType
	SettingsWriteAuthority    = cfgmodel.SettingsWriteAuthority
	ShellResolverOption       = envresolve.ShellResolverOption
	ToolGrep                  = cfgmodel.ToolGrep
	ToolLs                    = cfgmodel.ToolLs
	Tools                     = cfgmodel.Tools
	TrailerStyle              = cfgmodel.TrailerStyle
	TUIOptions                = cfgmodel.TUIOptions
	VariableResolver          = envresolve.VariableResolver
)

// Constants re-exported from the cfgmodel package.
const (
	AgentCoder                 = cfgmodel.AgentCoder
	AgentTask                  = cfgmodel.AgentTask
	DockerMCPName              = cfgmodel.DockerMCPName
	MCPHttp                    = cfgmodel.MCPHttp
	MCPOriginExternal          = cfgmodel.MCPOriginExternal
	MCPOriginGlobal            = cfgmodel.MCPOriginGlobal
	MCPOriginProject           = cfgmodel.MCPOriginProject
	MCPOriginSystem            = cfgmodel.MCPOriginSystem
	MCPOriginWorkspace         = cfgmodel.MCPOriginWorkspace
	MCPSourceExternal          = cfgmodel.MCPSourceExternal
	MCPSourceUser              = cfgmodel.MCPSourceUser
	MCPSSE                     = cfgmodel.MCPSSE
	MCPStdio                   = cfgmodel.MCPStdio
	ProxyNone                  = cfgmodel.ProxyNone
	ResponsesTransportAuto     = cfgmodel.ResponsesTransportAuto
	ResponsesTransportHTTP     = cfgmodel.ResponsesTransportHTTP
	ScopeGlobal                = cfgmodel.ScopeGlobal
	ScopeWorkspace             = cfgmodel.ScopeWorkspace
	SelectedModelTypeFast      = cfgmodel.SelectedModelTypeFast
	SelectedModelTypeReviewer  = cfgmodel.SelectedModelTypeReviewer
	SelectedModelTypeSmart     = cfgmodel.SelectedModelTypeSmart
	SelectedModelTypeWorker    = cfgmodel.SelectedModelTypeWorker
	SettingsAuthorityGuarded   = cfgmodel.SettingsAuthorityGuarded
	SettingsAuthorityTrustedUI = cfgmodel.SettingsAuthorityTrustedUI
	TrailerStyleAssistedBy     = cfgmodel.TrailerStyleAssistedBy
	TrailerStyleCoAuthoredBy   = cfgmodel.TrailerStyleCoAuthoredBy
	TrailerStyleNone           = cfgmodel.TrailerStyleNone
)

// Sentinel errors re-exported from the model packages.
var (
	ErrContextResolverUnsupported = envresolve.ErrContextResolverUnsupported
	ErrModelsEditedDirectly       = cfgmodel.ErrModelsEditedDirectly
	ErrNoWorkspaceConfig          = cfgmodel.ErrNoWorkspaceConfig
)

// Functions re-exported from the cfgmodel package.
func CustomRolesDocumentField(root map[string]json.RawMessage) (json.RawMessage, bool, error) {
	return cfgmodel.CustomRolesDocumentField(root)
}

func DecodeCustomRoles(raw json.RawMessage) (map[string]CustomRole, error) {
	return cfgmodel.DecodeCustomRoles(raw)
}

func EffectiveResponsesTransport(global, provider ResponsesTransport) ResponsesTransport {
	return cfgmodel.EffectiveResponsesTransport(global, provider)
}

func IsProxyNone(proxy string) bool {
	return cfgmodel.IsProxyNone(proxy)
}

func MatchRoleToolPattern(pattern, name string) bool {
	return cfgmodel.MatchRoleToolPattern(pattern, name)
}

func RoleToolsAllow(patterns []string, name string) bool {
	return cfgmodel.RoleToolsAllow(patterns, name)
}

func SetProviderHTTPClientFactory(factory ProviderHTTPClientFactory) func() {
	return cfgmodel.SetProviderHTTPClientFactory(factory)
}

func ValidateCustomRoleName(name string) error {
	return cfgmodel.ValidateCustomRoleName(name)
}

func ValidateCustomRoleSelection(cfg *Config, role CustomRole) error {
	return cfgmodel.ValidateCustomRoleSelection(cfg, role)
}

// Functions re-exported from the envresolve package.
func IdentityResolver() VariableResolver {
	return envresolve.IdentityResolver()
}

func NewShellVariableResolver(e env.Env, opts ...ShellResolverOption) VariableResolver {
	return envresolve.NewShellVariableResolver(e, opts...)
}

func ResolveValueContext(ctx context.Context, resolver VariableResolver, value string) (string, error) {
	return envresolve.ResolveValueContext(ctx, resolver, value)
}

func WithContext(ctx context.Context) ShellResolverOption {
	return envresolve.WithContext(ctx)
}

func WithExpander(e Expander) ShellResolverOption {
	return envresolve.WithExpander(e)
}

// Unexported forwarders: staying-root code called these unexported
// cfgmerge helpers before D2-1; each moved name keeps exactly one
// definition (in cfgmerge, exported) and this package forwards it.
func loadFromBytes(configs [][]byte) (*Config, error) {
	return cfgmerge.LoadFromBytes(configs)
}

func stripCustomRoleOverlays(configs [][]byte) ([][]byte, map[string]CustomRole, error) {
	return cfgmerge.StripCustomRoleOverlays(configs)
}

func validateCustomRoleWrite(raw []byte, keys []string) error {
	return cfgmerge.ValidateCustomRoleWrite(raw, keys)
}

func validateMCPDisabledOverlays(data []byte) error {
	return cfgmerge.ValidateMCPDisabledOverlays(data)
}

// ---------------------------------------------------------------------------
// D2-4 cluster 1: datadir group (domains/datadir)
//
// datadir.go, global_dir.go (+global_testguard.go), globalpaths.go,
// worktreehome.go, load_skills.go and the systemconfig pair moved to
// internal/config/domains/datadir. Everything below re-exports their API
// (design section 2.5 group 4) and forwards the unexported names that
// staying-root code and tests still call. ProjectConfigs keeps its
// original body here (NOT moved): its single statement calls the root
// lookupConfigs discovery (io/docread seam, still in load_files.go), so
// moving it would require a root-side logic move; design group 9 later
// relocates it to providers/catalogsrc.
// ---------------------------------------------------------------------------

// Constants re-exported from the datadir package.
const (
	AppName                  = cfgmodel.AppName
	DataDirSourceFlag        = datadir.DataDirSourceFlag
	DataDirSourceConfig      = datadir.DataDirSourceConfig
	DataDirSourceLegacyLocal = datadir.DataDirSourceLegacyLocal
	DataDirSourceShared      = datadir.DataDirSourceShared
	DataDirSourceDevIsolated = datadir.DataDirSourceDevIsolated
	DataDirSourceDefault     = datadir.DataDirSourceDefault
	GlobalDirEnv             = datadir.GlobalDirEnv
)

// Types re-exported from the datadir package.
type DataDirSource = datadir.DataDirSource

// Functions re-exported from the datadir package (group 4).
func EnsureGlobalDirTree() { datadir.EnsureGlobalDirTree() }

func GlobalCacheDir() string { return datadir.GlobalCacheDir() }

func GlobalConfig() string { return datadir.GlobalConfig() }

func GlobalConfigData() string { return datadir.GlobalConfigData() }

func GlobalDir() (string, bool) { return datadir.GlobalDir() }

func GlobalPromptSkillsDirs() []string { return datadir.GlobalPromptSkillsDirs() }

func GlobalSkillsDirs() []string { return datadir.GlobalSkillsDirs() }

func GlobalWorkspaceDir() string { return datadir.GlobalWorkspaceDir() }

func MarkSharedWorkspace(mainDataDir, wsRoot string) error {
	return datadir.MarkSharedWorkspace(mainDataDir, wsRoot)
}

// ProjectConfigs returns list of current project configs paths.
func ProjectConfigs(cwd string) []string {
	// Kept in root on purpose (see the D2-4 cluster-1 comment above):
	// byte-identical original body, still backed by root lookupConfigs.
	return lookupConfigs(cwd)
}

func ProjectPromptSkillsDirs(workingDir string) []string {
	return datadir.ProjectPromptSkillsDirs(workingDir)
}

func ProjectSkillsDir(workingDir string) []string { return datadir.ProjectSkillsDir(workingDir) }

func SystemConfig() string { return datadir.SystemConfig() }

func WorkspaceHome() bool { return datadir.WorkspaceHome() }

func WorkspaceRoot(dir string) string { return datadir.WorkspaceRoot(dir) }

// Unexported forwarders: staying-root code and tests still call these
// unexported datadir names; each moved name keeps exactly one definition
// (in datadir, exported via domains/datadir/export.go) and this package
// forwards it.
func canonicalConfigPath(path string) string { return datadir.CanonicalConfigPath(path) }

func cachePathFor(name string) string { return datadir.CachePathFor(name) }

func dataDirIsSharedOfLinkedWorktree(workingDir, dataDir string, source DataDirSource) bool {
	return datadir.DataDirIsSharedOfLinkedWorktree(workingDir, dataDir, source)
}

func defaultDataDirForWorkspace(workingDir string) (string, DataDirSource) {
	return datadir.DefaultDataDirForWorkspace(workingDir)
}

func isDevBuild() bool { return datadir.IsDevBuild() }

func projectBoundary(dir string) string { return datadir.ProjectBoundary(dir) }

func redirectionAllowed() bool { return datadir.RedirectionAllowed() }

func resolveDataDirectory(workingDir, dataDirFlag, dataDirFromConfig string) (string, DataDirSource) {
	return datadir.ResolveDataDirectory(workingDir, dataDirFlag, dataDirFromConfig)
}

func sameDir(a, b string) bool { return datadir.SameDir(a, b) }

func setDataDirHomeForTest(v bool) { datadir.SetDataDirHomeForTest(v) }

func setDataDirRedirectionOverride(v int) func() { return datadir.SetDataDirRedirectionOverride(v) }

func setOSExecutablePathForTest(path string) func() { return datadir.SetOSExecutablePathForTest(path) }

func sharedMarkerExists(mainDataDir, wsRoot string) bool {
	return datadir.SharedMarkerExists(mainDataDir, wsRoot)
}

const systemConfigPath = datadir.SystemConfigPath

func systemConfigSlot() string { return datadir.SystemConfigSlot() }

func worktreeRoot(dir string) string { return datadir.WorktreeRoot(dir) }

// worktreeRootCache seam forwarders: root discovery tests mutate the
// datadir-owned cache; the sync.Map itself is never exported.
func worktreeRootCacheDelete(key string) { datadir.WorktreeRootCacheDelete(key) }

func worktreeRootCacheLookup(key string) (string, bool) { return datadir.WorktreeRootCacheLookup(key) }

func worktreeRootCacheStore(key, value string) { datadir.WorktreeRootCacheStore(key, value) }

// White-box test forwarders (D2-4 cluster 1, datadir seams).
func detectDevBuild() bool { return datadir.DetectDevBuild() }

func linkedWorktreeInfo(wtRoot string) *datadir.LinkedWorktree {
	return datadir.LinkedWorktreeInfo(wtRoot)
}

func probeLinkedWorktree(wtRoot string) *datadir.LinkedWorktree {
	return datadir.ProbeLinkedWorktree(wtRoot)
}

func stripGitWorktreeEnv(cmd *exec.Cmd) { datadir.StripGitWorktreeEnv(cmd) }

func SharedWorkspaceMarkerPath(mainDataDir, wsRoot string) string {
	return datadir.SharedWorkspaceMarkerPath(mainDataDir, wsRoot)
}

func globalSkillsDirs(homeDir string) []string { return datadir.GlobalSkillsDirsForHome(homeDir) }

func projectSkillSubdirsLen() int { return datadir.ProjectSkillSubdirsLen() }

// Case-sensitivity re-exports from io/casefold.
type configCaseSensitivity = casefold.ConfigCaseSensitivity

const configCaseSensitivityUnknown = casefold.ConfigCaseSensitivityUnknown

func configPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return casefold.ConfigPlatformCaseFoldLeaf(dir, leaf)
}

func configPlatformCaseSensitivity(dir string) configCaseSensitivity {
	return casefold.ConfigPlatformCaseSensitivity(dir)
}

func configFoldCaseLeaf(leaf string, sensitivity configCaseSensitivity) (string, bool) {
	return casefold.ConfigFoldCaseLeaf(leaf, sensitivity)
}

// File-identity re-exports from io/fileid (vars preserve single error identity).
type configFileIdentity = fileid.FileIdentity

var (
	ErrConfigReloadUnstable = docread.ErrConfigReloadUnstable
	ErrConfigNonRegular     = fileid.ErrConfigNonRegular
	ErrConfigHardLink       = fileid.ErrConfigHardLink
	errConfigOwnerMismatch  = fileid.ErrConfigOwnerMismatch()
	errStableReadUnstable   = fileid.ErrStableReadUnstable()
)

func configFilePathIdentityMatches(path string, opened *os.File, openedInfo os.FileInfo) (bool, error) {
	return fileid.ConfigFilePathIdentityMatches(path, opened, openedInfo)
}

func configParentIdentity(path string) configFileIdentity {
	return fileid.ConfigParentIdentity(path)
}

func homeConfigOwner() int { return fileid.HomeConfigOwner() }

func systemConfigOwner() int { return fileid.SystemConfigOwner() }

func readOpenedConfigBytes(file *os.File) ([]byte, error) {
	return fileid.ReadOpenedConfigBytes(file)
}

func openStableConfigFile(path string) (*os.File, error) {
	return fileid.OpenStableConfigFile(path)
}

// Hook forwarding lives in io_forwarders.go (io/cfgcommit-backed).

// D2-4 API token, remote, and listener facade.
type (
	APITokenStore        = apitoken.APITokenStore
	APIToken             = apitoken.APIToken
	APITokenBinding      = apitoken.APITokenBinding
	APITokenPinBinding   = apitoken.APITokenPinBinding
	APITokenBatchBinding = apitoken.APITokenBatchBinding
	APIRemote            = apiremote.APIRemote
	ServerListener       = listeners.ServerListener
	ServerListenerTLS    = listeners.ServerListenerTLS
	ServerListenerLimits = listeners.ServerListenerLimits
	ServerListenerFlags  = listeners.ServerListenerFlags
)

var (
	ErrInvalidAPIToken      = apitoken.ErrInvalidAPIToken
	ErrAPITokenNotFound     = apitoken.ErrAPITokenNotFound
	ErrListenerConfigRead   = listeners.ErrListenerConfigRead
	ErrListenerConfigDecode = listeners.ErrListenerConfigDecode
)

func GlobalAPITokenStore() *APITokenStore { return apitoken.GlobalAPITokenStore() }

func APITokenScopeAllows(have []string, need string) bool {
	return apitoken.APITokenScopeAllows(have, need)
}

func ConsumeServeBootstrapToken(store *APITokenStore, getenv func(string) string, unsetenv func(string) error, now time.Time) (APIToken, bool, error) {
	return apitoken.ConsumeServeBootstrapToken(store, getenv, unsetenv, now)
}

func ConsumeServeBootstrapTokenFromEnv(store *APITokenStore, now time.Time) (APIToken, bool, error) {
	return apitoken.ConsumeServeBootstrapTokenFromEnv(store, now)
}

func CanonicalAPIWorkspace(path string) (string, error) {
	return apitoken.CanonicalAPIWorkspace(path)
}

func APITokenHash(token string) string { return apitoken.APITokenHash(token) }

func LooksLikeAPIToken(value string) bool { return apiremote.LooksLikeAPIToken(value) }

func ValidateAPIRemote(remote APIRemote) error { return apiremote.ValidateAPIRemote(remote) }

func ListAPIRemotes() (map[string]APIRemote, string, error) { return apiremote.ListAPIRemotes() }

func GetAPIRemote(name string) (APIRemote, error) { return apiremote.GetAPIRemote(name) }

func SetAPIRemote(name string, remote APIRemote, replace bool) error {
	return apiremote.SetAPIRemote(name, remote, replace)
}

func RemoveAPIRemote(name string) error { return apiremote.RemoveAPIRemote(name) }

func SetDefaultAPIRemote(name string) error { return apiremote.SetDefaultAPIRemote(name) }

func ResolveAPIRemote(selected string, getenv func(string) string) (string, APIRemote, error) {
	return apiremote.ResolveAPIRemote(selected, getenv)
}

func ReadAPIRemoteToken(remote APIRemote, getenv func(string) string) (string, error) {
	return apiremote.ReadAPIRemoteToken(remote, getenv)
}

func WriteAPIRemoteTokenFile(name string, token []byte) (string, error) {
	return apiremote.WriteAPIRemoteTokenFile(name, token)
}

func GlobalServerListeners(flags ServerListenerFlags) ([]ServerListener, error) {
	return listeners.GlobalServerListeners(flags)
}

func GlobalServerListenersWithSources(flags ServerListenerFlags) ([]ServerListener, []map[string]string, error) {
	return listeners.GlobalServerListenersWithSources(flags)
}

func init() {
	apiremote.SetGlobalConfigWriterFactory(func() (apiremote.GlobalConfigWriter, error) {
		return Load("", "", false)
	})
}

// ---------------------------------------------------------------------------
// X1 chunk 9a: settings lock component (store/settings)
//
// The root settings-lock implementation (settings_lock.go,
// settings_lock_sources.go and the per-platform
// settings_lock_sources_{windows,other}.go) moved to
// internal/config/store/settings. Everything below re-exports its API and
// forwards the unexported names that staying-root code and tests still
// call. settings_lock_hashes.go remains in the root temporarily (deleted
// next chunk) and is kept compiling by the freshSettingsHash forward.
// ---------------------------------------------------------------------------

// Sentinel errors re-exported from the settings package (vars preserve
// single error identity).
var (
	ErrSettingsLocked = settings.ErrSettingsLocked
	ErrWrongPassword  = settings.ErrWrongPassword
	ErrNotLocked      = settings.ErrNotLocked
)

// Types re-exported from the settings package.
type (
	SettingsEvent               = settings.SettingsEvent
	LockState                   = settings.LockState
	ErrSettingsLockUnrecognized = settings.ErrSettingsLockUnrecognized
)

// errSettingsLockKeyReserved: the settings component owns the reserved-key
// sentinel (settings.ErrSettingsLockKeyReserved()); root tests still assert
// against the unexported name.
var errSettingsLockKeyReserved = settings.ErrSettingsLockKeyReserved()

// syncRevKey is the settings component's semantic reserved key ("sync_rev");
// the root keeps a local literal copy (cfgmodel_locals precedent) for the
// remaining root tests.
const syncRevKey = "sync_rev"

// settingsWriteAudit: relocated from the deleted root settings_lock.go;
// store_mcp.go, store_mcp_transaction.go and store_write.go still use the
// unqualified name.
type settingsWriteAudit struct {
	event     *SettingsEvent
	refusedBy string
}

func HashPassword(password string) string { return settings.HashPassword(password) }

func VerifyPassword(password, stored string) bool { return settings.VerifyPassword(password, stored) }

func SetProcessPassword(password string) { settings.SetProcessPassword(password) }

func CheckPasswordAgainstDisk(password string) error {
	return settings.CheckPasswordAgainstDisk(password)
}

func SettingsWriteAllowed(workspaceDir string) error {
	return settings.SettingsWriteAllowed(workspaceDir)
}

// SetSettingsAuditSink forwards the sink registration; with the SettingsEvent
// alias, func(SettingsEvent) IS func(settings.SettingsEvent).
func SetSettingsAuditSink(f func(SettingsEvent)) { settings.SetSettingsAuditSink(f) }

func SetSettingsPasswordIterationsForTest(n int) (restore func()) {
	return settings.SetSettingsPasswordIterationsForTest(n)
}

// freshSettingsHash forward: the retained root settings_lock_hashes.go
// (dead after this chunk, deleted next chunk) still calls it unqualified.
func freshSettingsHash(path string) (string, error) {
	return settings.FreshSettingsHash(path)
}

// ---------------------------------------------------------------------------
// D2-3: catalogsrc group (providers/catalogsrc)
//
// provider.go, catwalk.go, hyper.go and catalog_supplement.go moved to
// internal/config/providers/catalogsrc. The four functions below forward
// their exported API so staying-root callers keep compiling unchanged.
// ---------------------------------------------------------------------------

// Providers returns the list of providers, taking into account cached results
// and whether or not auto update is enabled.
func Providers(cfg *Config) ([]catwalk.Provider, error) { return catalogsrc.Providers(cfg) }

// UpdateProviders updates the Catwalk providers list from a specified source.
func UpdateProviders(pathOrURL string) error { return catalogsrc.UpdateProviders(pathOrURL) }

// UpdateHyper updates the Hyper provider information from a specified URL.
func UpdateHyper(pathOrURL string) error { return catalogsrc.UpdateHyper(pathOrURL) }

// ResetProviderCacheForTests clears the process-wide provider/hyper sync
// singletons so tests in other packages can isolate themselves.
func ResetProviderCacheForTests() { catalogsrc.ResetProviderCacheForTests() }

// catalogsrc: confirmed-model verification and catalog cache.

// VerifyResult classifies how a provider answered a model-verification probe.
type VerifyResult = catalogsrc.VerifyResult

// ConfirmedModels is a provider roster accepted by a live verification.
type ConfirmedModels = catalogsrc.ConfirmedModels

const (
	Listed       = catalogsrc.Listed
	NotListed    = catalogsrc.NotListed
	Unverifiable = catalogsrc.Unverifiable
)

// ClearModelCatalogCache removes cached provider catalogs. An empty provider
// clears every supported catalog; the next Rush process fetches them again.
func ClearModelCatalogCache(provider string) error {
	return catalogsrc.ClearModelCatalogCache(provider)
}

// SetModelVerifyOnlineForTests allows verification requests in cache-only mode;
// it returns the restore function.
func SetModelVerifyOnlineForTests(on bool) func() { return catalogsrc.SetModelVerifyOnlineForTests(on) }

// VerifyProviderModel verifies that a provider's live roster lists modelID.
func VerifyProviderModel(ctx context.Context, cfg *Config, providerID, modelID string, resolvers ...VariableResolver) (VerifyResult, error) {
	return catalogsrc.VerifyProviderModel(ctx, cfg, providerID, modelID, resolvers...)
}

// RefreshProviderModels fetches the entire model roster, using a fresh cached roster when possible.
func RefreshProviderModels(ctx context.Context, cfg *Config, pc ProviderConfig, providerID string, resolver VariableResolver, modelID string) ([]catwalk.Model, string, error) {
	return catalogsrc.RefreshProviderModels(ctx, cfg, pc, providerID, resolver, modelID)
}

// FetchProviderModels fetches a provider's full roster without writing cache.
func FetchProviderModels(ctx context.Context, cfg *Config, pc ProviderConfig, providerID string, resolver VariableResolver) ([]catwalk.Model, string, error) {
	return catalogsrc.FetchProviderModels(ctx, cfg, pc, providerID, resolver)
}

// PersistConfirmedModels persists a successfully fetched full provider roster.
func PersistConfirmedModels(providerID, endpoint, key string, models []catwalk.Model) error {
	return catalogsrc.PersistConfirmedModels(providerID, endpoint, key, models)
}

// CachedConfirmedModels reports the matching roster and whether it is fresh.
func CachedConfirmedModels(pc ProviderConfig, providerID string, resolver VariableResolver) ([]catwalk.Model, bool) {
	return catalogsrc.CachedConfirmedModels(pc, providerID, resolver)
}

// provresolve: effective model selection.
func ConfigureSelectedModels(cfg *Config, knownProviders []catwalk.Provider) error {
	return provresolve.ConfigureSelectedModels(cfg, knownProviders)
}
