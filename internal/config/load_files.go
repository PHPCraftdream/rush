// Config-file discovery and merging: the bounded upward search for
// rush.json files, duplicate-workspace-config detection, and the JSON
// merge that folds every loaded document into one Config.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/qjebbs/go-jsons"
)

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated rush.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	// prepend default config paths
	configPaths := []string{
		systemConfigPath,
		GlobalConfig(),
		GlobalConfigData(),
	}

	configNames := []string{appName + ".json", "." + appName + ".json"}

	foundConfigs, err := fsext.LookupBounded(cwd, projectBoundary(cwd), configNames...)
	if err != nil {
		// returns at least default configs
		return configPaths
	}

	// reverse order so last config has more priority
	slices.Reverse(foundConfigs)

	return append(configPaths, foundConfigs...)
}

// pathAlreadyLoaded reports whether path (typically the computed workspace
// config path) is already present in loadedPaths (the paths loadFromConfigPaths
// already merged into cfg).
//
// Load and buildAndPublishReload both merge the workspace config
// (<DataDirectory>/rush.json) as a SEPARATE step after loadFromConfigPaths,
// under the assumption that it's a distinct file loadFromConfigPaths didn't
// already see. That assumption breaks when DataDirectory is configured (or,
// as in several tests, passed directly) such that the workspace path
// resolves to a path lookupConfigs already discovered and loaded — the file
// then gets read and merged a SECOND time via mustMarshalConfig(cfg)+wsData.
// jsons.Merge appends JSON arrays instead of overriding them (see
// internal/merge/ordered.go in github.com/qjebbs/go-jsons), so re-merging
// the same provider's "models" array against itself duplicates it, while
// scalar fields like models.smart.model are simply overwritten by the
// second read — the two reads of the same file are not even guaranteed to
// observe the same content if a concurrent writer lands between them,
// producing a config whose Models selection and whose Providers model list
// disagree (task #458). Skipping the second read/merge whenever the
// workspace path was already loaded closes this both structurally (no more
// double-processing of one file's content) and for the concurrent-write
// case (no second, possibly-different read of the same path).
func pathAlreadyLoaded(loadedPaths []string, path string) bool {
	clean := filepath.Clean(path)
	return slices.ContainsFunc(loadedPaths, func(p string) bool {
		return filepath.Clean(p) == clean
	})
}

func loadFromConfigPaths(configPaths []string) (*Config, []string, error) {
	var configs [][]byte
	var loaded []string

	for _, path := range configPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("failed to open config file %s: %w", path, err)
		}
		if len(data) == 0 {
			continue
		}
		if !json.Valid(data) {
			return nil, nil, fmt.Errorf("invalid JSON in config file %s", path)
		}
		configs = append(configs, data)
		loaded = append(loaded, path)
	}

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, err
	}
	return cfg, loaded, nil
}

func loadFromBytes(configs [][]byte) (*Config, error) {
	if len(configs) == 0 {
		return &Config{}, nil
	}

	data, err := jsons.Merge(configs)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	warnUnknownModelSlots(data)
	return &config, nil
}

// knownModelSlots is the exhaustive set of keys the "models" object in
// rush.json is read into (see the SelectedModelType* constants). Anything
// else under "models" is not a typo Go will catch: json.Unmarshal into
// Config.Models (map[SelectedModelType]SelectedModel) silently keeps an
// unrecognized key as a map entry that is simply never looked up anywhere
// -- Load never errors, "rush models state" just reports whatever the
// defaults resolve to, and the operator has no signal that the key they
// wrote did nothing. This bites hardest after a model-slot rename: a config
// file still holding a pre-rename slot key under "models" loads
// "successfully" and silently runs different models than the file claims.
// This function is a diagnostic ONLY -- it must never change
// which model gets selected, only warn when a written key cannot possibly
// affect that selection.
var knownModelSlots = map[string]bool{
	string(SelectedModelTypeSmart):    true,
	string(SelectedModelTypeFast):     true,
	string(SelectedModelTypeWorker):   true,
	string(SelectedModelTypeReviewer): true,
}

// warnUnknownModelSlots inspects the raw (pre-unmarshal) "models" object for
// keys outside knownModelSlots and logs one warning per offender. It reads
// the merged JSON bytes directly rather than the typed Config, because by
// the time the data reaches a map[SelectedModelType]SelectedModel field the
// unrecognized key has already been accepted as a valid (if unused) map
// entry -- there is nothing left in the typed value to detect the mistake
// from. Malformed "models" JSON is silently ignored here: loadFromBytes's
// own json.Unmarshal into Config already surfaces a real parse error to the
// caller, so this best-effort peek doesn't need to duplicate that.
func warnUnknownModelSlots(data []byte) {
	var probe struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return
	}
	for key := range probe.Models {
		if !knownModelSlots[key] {
			slog.Warn("unrecognized key under \"models\" in rush.json -- it is not one of the configured model slots and has no effect", "key", key, "known_slots", "smart, fast, worker, reviewer")
		}
	}
}
