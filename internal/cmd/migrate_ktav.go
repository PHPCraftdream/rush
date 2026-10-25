package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/spf13/cobra"
)

// applyLegacyRewrites is the pure, in-memory core of
// rewriteLegacyConfigContent: it parses a JSON config document, rewrites
// known legacy-named values in a small set of fields (disabled_skills,
// skills_paths, global_context_paths, data_directory), and re-marshals the
// document exactly as rewriteLegacyConfigContent has always written it
// (options re-marshal + json.MarshalIndent(top, "", "  ")). It performs no
// I/O. On any parse failure (input is not a JSON object, or options is not
// an object) it returns nil bytes, no notes, and changed=false; a
// successfully parsed document always yields the canonical re-marshaled
// bytes with changed reporting whether any legacy VALUE was rewritten, so a
// caller converting the document to another format can convert even when no
// rewrite was needed.
func applyLegacyRewrites(raw []byte) (newRaw []byte, notes []string, changed bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil, false
	}

	optionsRaw, ok := top["options"]
	if !ok {
		// A valid object with nothing to rewrite: hand the original bytes
		// back so a format conversion can still proceed.
		return raw, nil, false
	}

	var options map[string]json.RawMessage
	if err := json.Unmarshal(optionsRaw, &options); err != nil {
		return nil, nil, false
	}

	// disabled_skills: exact-match rewrite of known legacy skill IDs.
	if rawSkills, ok := options["disabled_skills"]; ok {
		var skills []string
		if err := json.Unmarshal(rawSkills, &skills); err == nil {
			rewritten := false
			for i, name := range skills {
				if newName, isLegacy := legacySkillRenames[name]; isLegacy {
					skills[i] = newName
					rewritten = true
					notes = append(notes, fmt.Sprintf("disabled_skills: %q -> %q", name, newName))
				}
			}
			if rewritten {
				if newRawSkills, err := json.Marshal(skills); err == nil {
					options["disabled_skills"] = newRawSkills
					changed = true
				}
			}
		}
	}

	// skills_paths, global_context_paths: path-segment-aware .crush ->
	// .rush rewrite, plus CRUSH.md -> RUSH.md for the context-file case.
	for _, field := range []string{"skills_paths", "global_context_paths"} {
		rawPaths, ok := options[field]
		if !ok {
			continue
		}
		var paths []string
		if err := json.Unmarshal(rawPaths, &paths); err != nil {
			continue
		}
		rewritten := false
		for i, p := range paths {
			newPath := rewriteLegacyConfigPath(p)
			if newPath != p {
				paths[i] = newPath
				rewritten = true
				notes = append(notes, fmt.Sprintf("%s: %q -> %q", field, p, newPath))
			}
		}
		if rewritten {
			if newRawPaths, err := json.Marshal(paths); err == nil {
				options[field] = newRawPaths
				changed = true
			}
		}
	}

	// data_directory: same .crush-path-segment rewrite, only if set.
	if rawDataDir, ok := options["data_directory"]; ok {
		var dataDir string
		if err := json.Unmarshal(rawDataDir, &dataDir); err == nil && dataDir != "" {
			newDataDir := rewriteLegacyConfigPath(dataDir)
			if newDataDir != dataDir {
				if newDataDirRaw, err := json.Marshal(newDataDir); err == nil {
					options["data_directory"] = newDataDirRaw
					changed = true
					notes = append(notes, fmt.Sprintf("data_directory: %q -> %q", dataDir, newDataDir))
				}
			}
		}
	}

	newOptionsRaw, err := json.Marshal(options)
	if err != nil {
		return nil, nil, false
	}
	top["options"] = newOptionsRaw

	marshaled, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, nil, false
	}
	return marshaled, notes, changed
}

// migrateToKtavTarget converts a legacy JSON config file into a Ktav
// document at target. The source file is never modified or removed unless
// the converted bytes are already on disk at target, so a conversion or
// write failure leaves the original crush.json in place.
func migrateToKtavTarget(cmd *cobra.Command, source, target string, prefix string) migrateStatus {
	raw, err := os.ReadFile(source)
	if err != nil {
		cmd.Printf("failed to rename %s%s  ->  %s: %v\n", formatPrefix(prefix), source, target, err)
		return statusFailed
	}

	newRaw, notes, _ := applyLegacyRewrites(raw)
	if newRaw == nil {
		// Not a JSON object we understand - still refuse to fabricate a Ktav
		// document from unknown bytes, but leave the source untouched.
		cmd.Printf("failed to rewrite content %s%s: not a JSON object\n", formatPrefix(prefix, "(content inside migrated config)"), source)
		return statusFailed
	}

	converted, err := ktavcodec.ConvertJSONToKtav(newRaw)
	if err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", formatPrefix(prefix, "(content inside migrated config)"), source, err)
		return statusFailed
	}

	// Preserve the target's existing permission bits where practical (this
	// file's own writes elsewhere - e.g. in tests seeding fixtures - use
	// 0o644, so that is the fallback when the target can't be stat'd for
	// some reason).
	perm := os.FileMode(0o644)
	if info, statErr := os.Stat(target); statErr == nil {
		perm = info.Mode().Perm()
	}

	// Write atomically: temp file in the SAME directory as target (so the
	// final os.Rename stays on one filesystem), then rename it over target.
	dir := filepath.Dir(target)
	tmpFile, err := os.CreateTemp(dir, filepath.Base(target)+".tmp-*")
	if err != nil {
		cmd.Printf("failed to rewrite content %s%s: %v\n", formatPrefix(prefix, "(content inside migrated config)"), target, err)
		return statusFailed
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(converted); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", formatPrefix(prefix, "(content inside migrated config)"), target, err)
		return statusFailed
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", formatPrefix(prefix, "(content inside migrated config)"), target, err)
		return statusFailed
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", formatPrefix(prefix, "(content inside migrated config)"), target, err)
		return statusFailed
	}

	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		cmd.Printf("failed to rewrite content %s%s: %v\n", formatPrefix(prefix, "(content inside migrated config)"), target, err)
		return statusFailed
	}

	cmd.Printf("renamed %s%s  ->  %s\n", formatPrefix(prefix), source, target)

	for _, note := range notes {
		cmd.Printf("rewrote %s%s: %s\n", formatPrefix(prefix, "(content inside migrated config)"), target, note)
	}

	// The converted bytes are on disk at target - only now may the source
	// go. A leftover crush.json beside rush.ktav is a duplicate config, so
	// failing to remove it must surface as an overall failure.
	if err := os.Remove(source); err != nil {
		cmd.Printf("failed to remove legacy source %s%s after conversion - remove it manually; a leftover crush.json beside the converted config is a duplicate config\n", formatPrefix(prefix), source)
		return statusFailed
	}

	return statusRenamed
}

// migrateKtavSibling returns the Ktav spelling of a legacy JSON config path:
// same directory, same base name, ".json" replaced with ".ktav". This mirrors
// config's unexported ktavSiblingOf; migrate constructs the sibling locally
// rather than reaching into unexported config internals.
func migrateKtavSibling(legacyJSON string) string {
	return strings.TrimSuffix(legacyJSON, ".json") + ".ktav"
}

// migrateExistingInnerConfig returns whichever spelling of a migrated inner
// config (rush.json or its rush.ktav sibling) already exists inside dir, or
// "" when neither does. Destination-conflict detection must treat EITHER
// spelling as an occupied slot: writing rush.json while rush.ktav is the
// active file (or vice versa) would create a second config copy the app —
// which selects exactly one slot — would silently ignore.
func migrateExistingInnerConfig(dir string) string {
	legacy := filepath.Join(dir, "rush.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	ktav := migrateKtavSibling(legacy)
	if _, err := os.Stat(ktav); err == nil {
		return ktav
	}
	return ""
}
