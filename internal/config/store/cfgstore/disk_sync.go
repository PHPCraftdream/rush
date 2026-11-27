// Package cfgstore: the config store engine (snapshots, reload, disk sync,
// MCP/OAuth write paths, settings staleness). Formed by a pure byte-preserving
// merge of the original 16 leaf files; see docs/plans/2026-11-26-cfgstore-ledger.tsv.

package cfgstore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
)

var (
	configDiskSyncInterval = 5 * time.Second
	configDiskSyncWait     = waitCodexRefresh
	configDiskSyncStat     = os.Stat
	// configDiskSyncFirstDelay draws the poller's first delay; injectable so
	// tests can pin boundary draws deterministically (no real sleeps).
	// Default: defaultConfigDiskSyncFirstDelay, whose bounds are pinned
	// directly by TestConfigDiskSyncJitterBounds and whose exact response
	// to boundary draws by TestConfigDiskSyncFirstDelayFormulaBoundaries
	// (via the configDiskSyncRandInt63 seam below the formula).
	configDiskSyncFirstDelay = defaultConfigDiskSyncFirstDelay
	// configDiskSyncRandInt63 is the draw source BELOW the first-delay
	// formula: the default formula's only nondeterminism. Stubbing it lets
	// tests pin the FORMULA's exact response to boundary draws (0 and
	// interval-1) without replacing the formula itself.
	configDiskSyncRandInt63 = rand.Int64N
	// configDiskSyncAfterTick is a test hook observing each completed tick.
	configDiskSyncAfterTick func(*ConfigStore, ConfigDiskSyncResult)
)

// configDiskSyncForcedEvery runs a forced exact check on every 12th tick
// (about 60 s) to catch same-size, same-mtime rewrites on coarse-mtime
// filesystems and tracked-path-set drift.
const configDiskSyncForcedEvery = 12

// ModelsChange describes one poll-detected change of the selected role
// models; the maps are the four role slots before and after the reload.
type ModelsChange struct {
	Before map[SelectedModelType]SelectedModel
	After  map[SelectedModelType]SelectedModel
}

// ConfigDiskSyncOptions configures StartConfigDiskSync. Reload defaults to
// ConfigStore.ReloadFromDisk; OnModelsChanged is optional.
type ConfigDiskSyncOptions struct {
	Reload          func(context.Context) error
	OnModelsChanged func(ModelsChange)
}

// ConfigDiskSyncResult reports what one poller tick did.
type ConfigDiskSyncResult struct {
	// Reloaded is true when a reload was attempted and succeeded.
	Reloaded bool
	// ReloadErr carries the reload error, if any.
	ReloadErr error
	// ModelsChanged is true when OnModelsChanged fired for this tick.
	ModelsChanged bool
}

// configDiskSyncState is the poller's private memory between ticks.
type configDiskSyncState struct {
	stats   map[string]os.FileInfo
	gen     uint64
	settled bool
	tick    uint64
	// force skips the stat gate (PollConfigDiskOnce).
	force bool
	// failedKey remembers the digest set of the last failed reload; no
	// retry or repeated warning happens while the digests stay identical.
	failedKey string
	// lastAttemptKey remembers the candidate digest set of the last
	// ATTEMPT, whatever its outcome: ticks that would repeat an identical
	// attempt stay quiescent, which closes the ineligible-tracked-file
	// loop where the digests can never match the published snapshot.
	lastAttemptKey string
	// reloadAttempts counts reload attempts (test observability).
	reloadAttempts uint64
}

// StartConfigDiskSync starts the config disk poller. It is idempotent, a
// no-op for a store without a working directory (library/test store), and
// refused permanently once StopConfigDiskSync has been called on the store
// (terminal stop, even when nothing was ever started).
func (s *ConfigStore) StartConfigDiskSync(opts ConfigDiskSyncOptions) {
	s.configSyncMu.Lock()
	defer s.configSyncMu.Unlock()
	// A store with nothing tracked has nothing to poll (library mode).
	if s.workingDir == "" || s.configSyncStarted || s.configSyncStopped || len(s.loadSnapshot().trackedConfigPaths) == 0 {
		return
	}
	if s.configSyncCtx == nil {
		s.configSyncCtx, s.configSyncCancel = context.WithCancel(context.Background())
	}
	if s.configSyncCtx.Err() != nil {
		return
	}
	if opts.Reload == nil {
		opts.Reload = s.ReloadFromDisk
	}
	s.configSyncOpts = opts
	s.configSyncStarted = true
	s.configSyncWG.Add(1)
	go s.runConfigDiskSync(s.configSyncCtx)
}

// StopConfigDiskSync cancels the poller and joins its goroutine. It is
// idempotent and TERMINAL: the stopped state is recorded even when nothing
// was ever started, and every later StartConfigDiskSync on the store is
// refused (a cancelled-context check alone would miss the Stop-before-Start
// case, where a fresh context would otherwise be created). The mutex is
// released BEFORE the join: a successful in-flight reload calls
// configSyncOnModelsChangedFunc, which takes configSyncMu on the poller
// goroutine — holding the lock across WG.Wait would deadlock Stop against
// that reload.
func (s *ConfigStore) StopConfigDiskSync() {
	s.configSyncMu.Lock()
	s.configSyncStopped = true
	if s.configSyncCancel != nil {
		s.configSyncCancel()
	}
	s.configSyncMu.Unlock()
	s.configSyncWG.Wait()
}

// PollConfigDiskOnce runs one synchronous tick with a forced exact check,
// regardless of file metadata. It is safe alongside or instead of the
// background poller.
func (s *ConfigStore) PollConfigDiskOnce(ctx context.Context) (ConfigDiskSyncResult, error) {
	var st configDiskSyncState
	st.force = true
	return s.syncConfigFromDisk(ctx, &st)
}

// defaultConfigDiskSyncFirstDelay is the production first-delay draw:
// uniform in [0, configDiskSyncInterval), so N concurrent processes do not
// tick in lockstep. Named so tests can pin its per-draw bounds directly
// while the configDiskSyncFirstDelay seam stays test-injectable.
func defaultConfigDiskSyncFirstDelay() time.Duration {
	return time.Duration(configDiskSyncRandInt63(int64(configDiskSyncInterval)))
}

func configDiskSyncDelay() time.Duration {
	spread := configDiskSyncInterval / 5
	return configDiskSyncInterval - spread + time.Duration(rand.Int64N(int64(2*spread)+1))
}

func (s *ConfigStore) runConfigDiskSync(ctx context.Context) {
	defer s.configSyncWG.Done()
	var st configDiskSyncState
	// First delay is uniform in [0, interval) so N processes do not tick
	// in lockstep.
	delay := configDiskSyncFirstDelay()
	for configDiskSyncWait(ctx, delay) == nil {
		if ctx.Err() != nil {
			return
		}
		result, _ := s.syncConfigFromDisk(ctx, &st)
		if hook := configDiskSyncAfterTick; hook != nil {
			hook(s, result)
		}
		delay = configDiskSyncDelay()
	}
}

func (s *ConfigStore) syncConfigFromDisk(ctx context.Context, st *configDiskSyncState) (ConfigDiskSyncResult, error) {
	var result ConfigDiskSyncResult
	if ctx.Err() != nil || s.workingDir == "" {
		return result, nil
	}
	sn := s.loadSnapshot()
	if sn.config == nil || len(sn.trackedConfigPaths) == 0 {
		return result, nil
	}
	st.tick++
	forced := st.force || st.tick%configDiskSyncForcedEvery == 0
	stats := make(map[string]os.FileInfo, len(sn.trackedConfigPaths))
	statChanged := false
	for _, path := range sn.trackedConfigPaths {
		fi, err := configDiskSyncStat(path)
		if err != nil && !os.IsNotExist(err) {
			// A Windows sharing violation during an atomic replace makes
			// the tick unsettled; the next tick retries. A not-yet-existing
			// tracked file is a valid stat (fi stays nil).
			st.settled = false
			return result, nil
		}
		stats[path] = fi
		// A nil FileInfo means the tracked file does not exist; nil equals
		// nil (still absent), and any transition counts as a change.
		if prev, seen := st.stats[path]; !seen ||
			(prev == nil) != (fi == nil) ||
			(prev != nil && (!os.SameFile(prev, fi) || prev.Size() != fi.Size() || !prev.ModTime().Equal(fi.ModTime()))) {
			statChanged = true
		}
	}
	st.stats = stats
	// Path-set drift (a candidate became eligible or ineligible, e.g. a
	// project boundary change) is itself a change: it must trigger a reload
	// by itself even when every previously tracked file is byte-identical,
	// because the digest gate only examines the OLD tracked paths.
	drift := forced && !docread.SameReloadPathSet(configDiskSyncCandidatePaths(s, sn), sn.trackedConfigPaths)
	if !drift && !forced && st.settled && !statChanged && st.gen == sn.generation {
		return result, nil
	}
	st.gen = sn.generation
	// A FAILED drift reload leaves the candidate set still differing, so
	// drift stays true on later forced ticks. configDiskSyncReload memoizes
	// the failure by candidate-set key, so those ticks stay quiescent (no
	// retry, no repeated warning) until a candidate digest or the candidate
	// set itself changes.
	if !drift && configDiskSyncDigestsMatch(sn) {
		// Snapshot and disk agree: any earlier attempt memo is stale from
		// here on — another reload path may have published exactly this
		// disk state, and a later flip BACK to the attempted content must
		// still reload (P2-2: without the clear, A→B→A skips forever).
		st.lastAttemptKey = ""
		st.settled = true
		return result, nil
	}
	st.settled = false
	return s.configDiskSyncReload(ctx, st, sn, &result)
}

// configDiskSyncReload runs the injected reload once, with the failure
// bookkeeping of plan section 3.8: no retry and one warning per distinct
// failed candidate key (candidate path set plus digests), one info on
// recovery. An outcome-independent memo of the last attempted key keeps
// ticks with an unchanged candidate set from repeating an attempt whose
// result cannot differ (e.g. a tracked file the loader always ignores).
func (s *ConfigStore) configDiskSyncReload(ctx context.Context, st *configDiskSyncState, before *storeSnapshot, result *ConfigDiskSyncResult) (ConfigDiskSyncResult, error) {
	key := configDiskSyncDigestKey(s, before)
	if st.failedKey == key {
		// Known-bad content: the current snapshot stays published and the
		// warning was already logged for this exact digest set.
		return *result, nil
	}
	if st.lastAttemptKey == key {
		// The same candidate set was already attempted; whether that
		// attempt succeeded or failed, an unchanged candidate set cannot
		// produce a different outcome, so skip without a retry, a new
		// warning, or another attempt.
		return *result, nil
	}
	beforeModels := cloneModelsSelections(before.config.Models)
	st.reloadAttempts++
	err := s.configSyncReloadFunc()(ctx)
	st.lastAttemptKey = key
	if err != nil {
		st.failedKey = key
		result.ReloadErr = err
		slog.Warn("Config disk sync reload failed; will retry when the files change again", "err", err)
		return *result, nil
	}
	if st.failedKey != "" {
		slog.Info("Config disk sync recovered after a failed reload")
	}
	st.failedKey = ""
	result.Reloaded = true
	if after := s.loadSnapshot(); !configDiskModelsEqual(before.config, after.config) {
		if onChanged := s.configSyncOnModelsChangedFunc(); onChanged != nil {
			result.ModelsChanged = true
			onChanged(ModelsChange{Before: beforeModels, After: cloneModelsSelections(after.config.Models)})
		}
	}
	return *result, nil
}

// ConfigDiskSyncRunning reports whether the config disk-sync poller
// goroutine is running: started, not stopped, and not refused after a stop.
func (s *ConfigStore) ConfigDiskSyncRunning() bool {
	s.configSyncMu.Lock()
	defer s.configSyncMu.Unlock()
	return s.configSyncStarted && !s.configSyncStopped && s.configSyncCtx != nil && s.configSyncCtx.Err() == nil
}

// configDiskSyncCandidatePaths rebuilds the current config candidate path
// set, for the forced tick's drift check against the tracked set.
func configDiskSyncCandidatePaths(s *ConfigStore, sn *storeSnapshot) []string {
	paths := docread.LookupConfigCandidates(s.workingDir)
	paths = append(paths, docread.McpJSONCandidatePaths(s.workingDir)...)
	paths = append(paths, sn.workspacePath, s.globalDataPath)
	// Both spellings of each logical slot, so the tracked set (built the
	// same way) never drifts spuriously and slot-selection flips stay
	// visible on natural ticks.
	return docread.ExpandConfigSlotSiblings(paths)
}

// configDiskSyncDigestsMatch compares every tracked path's current content
// digest against the published snapshot's fingerprint, on exists and digest
// only (metadata-only changes do not trigger a reload).
func configDiskSyncDigestsMatch(sn *storeSnapshot) bool {
	for _, path := range sn.trackedConfigPaths {
		fingerprint, err := docread.ReadReloadFingerprint(path)
		if err != nil {
			return false
		}
		snapshot, ok := sn.snapshots[path]
		if !ok || fingerprint.Exists != snapshot.Exists || fingerprint.Digest != snapshot.Fingerprint().Digest {
			return false
		}
	}
	return true
}

// configDiskSyncDigestKey builds a stable key over the current CANDIDATE
// path set and each candidate's exists flag and content digest. It must
// cover the candidate set, not just the previously tracked paths: a drift
// reload failing on a newly eligible file must retry once that file is
// fixed, and fixing it changes no previously tracked digest. Paths are
// normalized with docread.NormalizeDiscoveryPath (the same normalization the
// tracked set is keyed by) and sorted, so the key is stable across ticks.
func configDiskSyncDigestKey(s *ConfigStore, sn *storeSnapshot) string {
	candidates := slices.Clone(configDiskSyncCandidatePaths(s, sn))
	for i, path := range candidates {
		candidates[i] = docread.NormalizeDiscoveryPath(path)
	}
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)
	var key strings.Builder
	for _, path := range candidates {
		fingerprint, err := docread.ReadReloadFingerprint(path)
		if err != nil {
			key.WriteString(path + "\x00err\x00")
			continue
		}
		if fingerprint.Exists {
			key.WriteString(path + "\x001\x00" + hex.EncodeToString(fingerprint.Digest[:]) + "\x00")
		} else {
			key.WriteString(path + "\x000\x00")
		}
	}
	sum := sha256.Sum256([]byte(key.String()))
	return hex.EncodeToString(sum[:])
}

// configDiskModelProfile is the compared projection of one role slot plus
// its referenced provider identity: the COMPLETE effective selection
// (provider, model, reasoning effort, thinking flag, token budget,
// temperature, top-p, top-k, frequency/presence penalties, provider
// options), so ANY single-field change still diffs, not just the identifier.
type configDiskModelProfile struct {
	provider string
	model    string
	effort   string
	tier     string
	think    bool
	maxTk    int64
	// Pointer-valued SelectedModel fields are compared by dereferenced
	// value via the set/value pairs below: nil vs non-nil differ (set
	// false vs true), equal values compare equal.
	tempSet bool
	temp    float64
	topPSet bool
	topP    float64
	topKSet bool
	topK    int64
	freqSet bool
	freq    float64
	presSet bool
	pres    float64
	// ProviderOptions compared by canonical JSON encoding (encoding/json
	// sorts map keys); nil and the empty map both encode to "" and compare
	// EQUAL, so key-order noise and empty maps do not cause spurious
	// callbacks.
	optsJSON string
	id       string
	typ      string
}

// optFloat normalizes a *float64 override to a comparable set/value pair.
func optFloat(v *float64) (bool, float64) {
	if v == nil {
		return false, 0
	}
	return true, *v
}

// optInt normalizes a *int64 override to a comparable set/value pair.
func optInt(v *int64) (bool, int64) {
	if v == nil {
		return false, 0
	}
	return true, *v
}

// providerOptionsJSON canonicalizes a ProviderOptions map for comparison;
// nil and empty both yield "".
func providerOptionsJSON(opts map[string]any) string {
	if len(opts) == 0 {
		return ""
	}
	data, err := json.Marshal(opts)
	if err != nil {
		// Values come from JSON configs, so this is unreachable in practice;
		// fall back to a distinct key rather than silently equating.
		return "<unencodable>"
	}
	return string(data)
}

// configDiskModelsEqual reports whether the four role slots and the
// providers they reference are identical between two configs.
func configDiskModelsEqual(before, after *Config) bool {
	return reflect.DeepEqual(configDiskModelsProfile(before), configDiskModelsProfile(after))
}

func configDiskModelsProfile(cfg *Config) map[SelectedModelType]configDiskModelProfile {
	profile := make(map[SelectedModelType]configDiskModelProfile, 4)
	for _, slot := range []SelectedModelType{SelectedModelTypeSmart, SelectedModelTypeFast, SelectedModelTypeWorker, SelectedModelTypeReviewer} {
		var entry configDiskModelProfile
		if cfg != nil {
			sel := cfg.Models[slot]
			entry = configDiskModelProfile{
				provider: sel.Provider,
				model:    sel.Model,
				effort:   sel.ReasoningEffort,
				tier:     sel.ServiceTier,
				think:    sel.Think,
				maxTk:    sel.MaxTokens,
			}
			entry.tempSet, entry.temp = optFloat(sel.Temperature)
			entry.topPSet, entry.topP = optFloat(sel.TopP)
			entry.topKSet, entry.topK = optInt(sel.TopK)
			entry.freqSet, entry.freq = optFloat(sel.FrequencyPenalty)
			entry.presSet, entry.pres = optFloat(sel.PresencePenalty)
			entry.optsJSON = providerOptionsJSON(sel.ProviderOptions)
			if entry.provider != "" && cfg.Providers != nil {
				if pc, ok := cfg.Providers.Get(entry.provider); ok {
					entry.id, entry.typ = pc.ID, string(pc.Type)
				}
			}
		}
		profile[slot] = entry
	}
	return profile
}

func cloneModelsSelections(models map[SelectedModelType]SelectedModel) map[SelectedModelType]SelectedModel {
	if models == nil {
		return nil
	}
	return maps.Clone(models)
}

func (s *ConfigStore) configSyncReloadFunc() func(context.Context) error {
	s.configSyncMu.Lock()
	defer s.configSyncMu.Unlock()
	if s.configSyncOpts.Reload != nil {
		return s.configSyncOpts.Reload
	}
	return s.ReloadFromDisk
}

func (s *ConfigStore) configSyncOnModelsChangedFunc() func(ModelsChange) {
	s.configSyncMu.Lock()
	defer s.configSyncMu.Unlock()
	return s.configSyncOpts.OnModelsChanged
}

var errReloadDiskChanged = errors.New("config files changed while reloading")

func candidateInputsChanged(workingDir string, configPaths, externalPaths []string, fingerprints map[string]reloadFileFingerprint) bool {
	return !docread.SameReloadPathSet(configPaths, docread.LookupConfigCandidates(workingDir)) ||
		!docread.SameReloadPathSet(externalPaths, docread.McpJSONCandidatePaths(workingDir)) ||
		docread.ReloadFingerprintsChangedWithOwner(fingerprints, func(path string) (int, bool, error) {
			globalExternal := filepath.Join(home.Dir(), ".claude", ".mcp.json")
			if docread.NormalizeReloadPath(path) == docread.NormalizeReloadPath(globalExternal) {
				return homeConfigOwner(), true, nil
			}
			return docread.ConfigOwnerPolicyForPath(path, workingDir)
		})
}

func initialMCPRevisions(cfg *cfgmodel.Config) map[string]uint64 {
	result := make(map[string]uint64)
	if cfg != nil {
		for name := range cfg.MCP {
			result[name] = 1
		}
	}
	return result
}

// mcpInputFingerprints fingerprints only the MCP entries in each source
// document. Changes to unrelated config fields therefore preserve an entry's
// revision across reloads.
func mcpInputFingerprints(rush, external []stableConfigDocument) map[string][sha256.Size]byte {
	type document struct {
		path      string
		entries   map[string]json.RawMessage
		rawDigest []byte
	}
	documentsByPath := make(map[string]document, len(rush)+len(external))
	names := make(map[string]struct{})
	addDocument := func(sourceDocument stableConfigDocument, entries map[string]json.RawMessage, rawDigest []byte) {
		path := docread.NormalizeReloadPath(sourceDocument.Path)
		if path == "" {
			path = docread.NormalizeDiscoveryPath(sourceDocument.Path)
		}
		if existing, ok := documentsByPath[path]; ok {
			if len(existing.entries) == 0 && len(entries) != 0 {
				documentsByPath[path] = document{path: path, entries: entries, rawDigest: rawDigest}
			}
			return
		}
		documentsByPath[path] = document{path: path, entries: entries, rawDigest: rawDigest}
	}
	for _, sourceDocument := range rush {
		var rawDigest []byte
		if docread.IsKtavConfigPath(sourceDocument.Path) && sourceDocument.Fingerprint.Exists {
			rawDigest = append([]byte(nil), sourceDocument.Fingerprint.Digest[:]...)
		}
		var root struct {
			MCP map[string]json.RawMessage `json:"mcp"`
		}
		if json.Unmarshal(sourceDocument.Data, &root) == nil {
			addDocument(sourceDocument, root.MCP, rawDigest)
			for name := range root.MCP {
				names[name] = struct{}{}
			}
		}
	}
	for _, sourceDocument := range external {
		var root struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if json.Unmarshal(sourceDocument.Data, &root) == nil {
			addDocument(sourceDocument, root.MCPServers, nil)
			for name := range root.MCPServers {
				names[name] = struct{}{}
			}
		}
	}
	documents := make([]document, 0, len(documentsByPath))
	for _, document := range documentsByPath {
		documents = append(documents, document)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].path < documents[j].path })
	result := make(map[string][sha256.Size]byte, len(names))
	for name := range names {
		hash := sha256.New()
		for _, document := range documents {
			path := docread.NormalizeReloadPath(document.path)
			if path == "" {
				path = docread.NormalizeDiscoveryPath(document.path)
			}
			hash.Write([]byte(path))
			if raw, ok := document.entries[name]; ok {
				if len(document.rawDigest) > 0 {
					hash.Write(document.rawDigest)
				}
				hash.Write([]byte{1})
				hash.Write(raw)
			} else {
				hash.Write([]byte{0})
			}
		}
		var digest [sha256.Size]byte
		copy(digest[:], hash.Sum(nil))
		result[name] = digest
	}
	return result
}

// mcpRevisionDiff advances only names whose effective definition or exact
// source input changed. A missing revision is deliberately treated as zero so
// an in-memory staged entry receives a real admission token.
func mcpRevisionDiff(current map[string]uint64, previous, next *cfgmodel.Config, previousInputs, nextInputs map[string][sha256.Size]byte, force ...string) map[string]uint64 {
	result := maps.Clone(current)
	if result == nil {
		result = make(map[string]uint64)
	}
	names := make(map[string]struct{})
	for name := range mcpNames(previous) {
		names[name] = struct{}{}
	}
	for name := range mcpNames(next) {
		names[name] = struct{}{}
	}
	for name := range previousInputs {
		names[name] = struct{}{}
	}
	for name := range nextInputs {
		names[name] = struct{}{}
	}
	for _, name := range force {
		if name != "" {
			names[name] = struct{}{}
		}
	}
	forced := make(map[string]struct{}, len(force))
	for _, name := range force {
		forced[name] = struct{}{}
	}
	for name := range names {
		previousConfig, previousExists := mcpConfig(previous, name)
		nextConfig, nextExists := mcpConfig(next, name)
		previousInput, previousHasInput := previousInputs[name]
		nextInput, nextHasInput := nextInputs[name]
		if _, ok := forced[name]; ok || previousExists != nextExists ||
			previousExists && !reflect.DeepEqual(previousConfig, nextConfig) ||
			previousHasInput != nextHasInput ||
			previousHasInput && nextHasInput && previousInput != nextInput {
			result[name]++
		}
	}
	return result
}

func mcpNames(cfg *cfgmodel.Config) map[string]cfgmodel.MCPConfig {
	if cfg == nil {
		return nil
	}
	return cfg.MCP
}

func loadConfigCandidateStableForWorkingDir(configPaths []string, workingDir string) (*cfgmodel.Config, []string, map[string]reloadFileFingerprint, []stableConfigDocument, error) {
	documents, err := docread.ReadStableConfigDocumentsWithOwner(configPaths, func(path string) (int, bool, error) {
		return docread.ConfigOwnerPolicyForPath(path, workingDir)
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, document := range documents {
		if document.Present && len(document.Data) > 0 && !json.Valid(document.Data) {
			return nil, nil, nil, nil, fmt.Errorf("invalid JSON in config file %s", document.Path)
		}
		if document.Present {
			if err := validateMCPDisabledOverlays(document.Data); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("invalid MCP configuration in config file %s: %w", document.Path, err)
			}
		}
	}
	configs, loaded, fingerprints := docread.ConfigDocumentBytes(documents)
	docread.AddDocumentAliasFingerprints(fingerprints, documents)

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, loaded, fingerprints, documents, nil
}

// setDefaults fills in every option, path, and model-map field that the
// merged config files left unset (root load_defaults.go).
func setDefaults(c *cfgmodel.Config, workingDir, dataDir string) {
	if c.Options == nil {
		c.Options = &cfgmodel.Options{}
	}
	if c.Options.TUI == nil {
		c.Options.TUI = &cfgmodel.TUIOptions{}
	}
	if len(c.Options.GlobalContextPaths) == 0 {
		rushConfigDir := filepath.Dir(datadir.GlobalConfig())
		c.Options.GlobalContextPaths = []string{
			filepath.Join(rushConfigDir, "RUSH.md"),
			filepath.Join(filepath.Dir(rushConfigDir), "AGENTS.md"),
		}
	}
	slices.Sort(c.Options.GlobalContextPaths)
	c.Options.GlobalContextPaths = slices.Compact(c.Options.GlobalContextPaths)

	if dataDir != "" {
		c.Options.DataDirectory = dataDir
	} else if c.Options.DataDirectory == "" {
		if path, ok := fsext.LookupClosestBounded(workingDir, projectBoundary(workingDir), defaultDataDirectory); ok {
			c.Options.DataDirectory = path
		} else {
			c.Options.DataDirectory = filepath.Join(workingDir, defaultDataDirectory)
		}
	}
	c.Options.DataDirectory = filepath.Clean(filepathext.SmartJoin(workingDir, c.Options.DataDirectory))
	if c.Providers == nil {
		c.Providers = csync.NewMap[string, cfgmodel.ProviderConfig]()
	}
	if c.Models == nil {
		c.Models = make(map[cfgmodel.SelectedModelType]cfgmodel.SelectedModel)
	}
	if c.RecentModels == nil {
		c.RecentModels = make(map[cfgmodel.SelectedModelType][]cfgmodel.SelectedModel)
	}
	if c.MCP == nil {
		c.MCP = make(map[string]cfgmodel.MCPConfig)
	}

	// Add the default context paths if they are not already present
	c.Options.ContextPaths = append(slices.Clone(defaultContextPaths), c.Options.ContextPaths...)
	slices.Sort(c.Options.ContextPaths)
	c.Options.ContextPaths = slices.Compact(c.Options.ContextPaths)

	// Add the prompt-advertised default skills directories if not already
	// present.
	for _, dir := range datadir.GlobalPromptSkillsDirs() {
		if !slices.Contains(c.Options.SkillsPaths, dir) {
			c.Options.SkillsPaths = append(c.Options.SkillsPaths, dir)
		}
	}

	// Project specific skills dirs, prompt-advertised subset.
	c.Options.SkillsPaths = append(c.Options.SkillsPaths, datadir.ProjectPromptSkillsDirs(workingDir)...)

	if str, ok := os.LookupEnv("RUSH_DISABLE_PROVIDER_AUTO_UPDATE"); ok {
		c.Options.DisableProviderAutoUpdate, _ = strconv.ParseBool(str)
	}

	if str, ok := os.LookupEnv("RUSH_DISABLE_DEFAULT_PROVIDERS"); ok {
		c.Options.DisableDefaultProviders, _ = strconv.ParseBool(str)
	}

	if c.Options.Attribution == nil {
		c.Options.Attribution = &cfgmodel.Attribution{
			TrailerStyle:  cfgmodel.TrailerStyleAssistedBy,
			GeneratedWith: true,
		}
	} else if c.Options.Attribution.TrailerStyle == "" {
		// Migrate deprecated co_authored_by or apply default
		if c.Options.Attribution.CoAuthoredBy != nil {
			if *c.Options.Attribution.CoAuthoredBy {
				c.Options.Attribution.TrailerStyle = cfgmodel.TrailerStyleCoAuthoredBy
			} else {
				c.Options.Attribution.TrailerStyle = cfgmodel.TrailerStyleNone
			}
		} else {
			c.Options.Attribution.TrailerStyle = cfgmodel.TrailerStyleAssistedBy
		}
	}
	c.Options.InitializeAs = cmp.Or(c.Options.InitializeAs, defaultInitializeAs)
}

func projectBoundary(dir string) string { return datadir.ProjectBoundary(dir) }

// mustMarshalConfig marshals the config to JSON bytes, returning empty JSON on
// error (root load.go:392).
func mustMarshalConfig(cfg *cfgmodel.Config) []byte {
	data, err := json.Marshal(cfg)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// resolverInputFingerprint records the semantic input to the shell resolver
// (root cfgmodel_locals.go:128).
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

// appName and defaultDataDirectory are byte-identical local copies of the
// cfgmodel declarations (root cfgmodel_locals.go precedent).
const (
	appName              = cfgmodel.AppName
	defaultDataDirectory = ".rush"
	defaultInitializeAs  = "AGENTS.md"
)

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

const maxRecentModelsPerType = 5

// Test-hook seam replication of root config_test_hooks.go: the leaf keeps its
// own nil-default hook slots and guarded runner functions with the exact
// root nil-guard semantics.

var (
	// afterMCPResolveTarget is the test-only seam between target resolution
	// and sidecar acquisition (root-only seam, replicated for the leaf).
	afterMCPResolveTarget func(*configWriteTarget)
	// beforeMCPTargetBindingVerification is the test-only seam before MCP
	// target binding verification (root-only seam, replicated for the leaf).
	beforeMCPTargetBindingVerification func(*configWriteTarget)
)

// runConfigAfterMCPResolveTargetHook is the leaf seam: the production MCP
// admission path calls it with the leaf configWriteTarget.
func runConfigAfterMCPResolveTargetHook(target *configWriteTarget) {
	if afterMCPResolveTarget != nil {
		afterMCPResolveTarget(target)
	}
}

// runConfigBeforeMCPTargetBindingVerificationHook is the leaf seam: the
// production write path calls it with the leaf configWriteTarget.
func runConfigBeforeMCPTargetBindingVerificationHook(target *configWriteTarget) {
	if beforeMCPTargetBindingVerification != nil {
		beforeMCPTargetBindingVerification(target)
	}
}

// ConfigWriteTarget is the exported alias of the leaf write-target struct
// (root store_write.go configWriteTarget). The root package aliases it back
// as config.WriteTarget plumbing and registers its test hooks through
// SetMCPTargetHooks, so the root's configWriteTarget spelling keeps working.
type ConfigWriteTarget = configWriteTarget

// SetMCPTargetHooks installs the root package's test-only MCP seams into the
// leaf's nil-default hook slots (leaf replica of root config_test_hooks.go).
func SetMCPTargetHooks(after func(*ConfigWriteTarget), before func(*ConfigWriteTarget)) {
	afterMCPResolveTarget = after
	beforeMCPTargetBindingVerification = before
}
