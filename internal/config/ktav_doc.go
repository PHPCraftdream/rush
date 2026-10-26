// Format-aware settings document layer: a .ktav config file is only the
// DISK representation of the JSON-shaped in-memory model. Every
// read-modify-write reads raw bytes plus a raw fingerprint, decodes Ktav
// to JSON-shaped bytes for the semantic work, and encodes back to the
// target format before committing against the ORIGINAL raw fingerprint.
package config

import (
	"fmt"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
)

// settingsDocumentFormat distinguishes the disk representations a settings
// document can use: the legacy JSON spelling and the Ktav spelling.
type settingsDocumentFormat int

const (
	// settingsDocumentJSON is the legacy rush.json disk representation.
	settingsDocumentJSON settingsDocumentFormat = iota
	// settingsDocumentKtav is the rush.ktav disk representation.
	settingsDocumentKtav
)

// settingsDocument pairs one settings file's raw on-disk bytes with the
// JSON-shaped bytes every guard, merge, and mutation operates on. The raw
// fingerprint is bound to the RAW bytes as read; it must never be computed
// from (or compared against) the normalized JSON bytes.
type settingsDocument struct {
	path           string
	jsonBytes      []byte
	rawBytes       []byte
	rawFingerprint reloadFileFingerprint
	format         settingsDocumentFormat
}

// settingsDocumentFormatOf reports the disk format a path's extension names.
func settingsDocumentFormatOf(path string) settingsDocumentFormat {
	if isKtavConfigPath(path) {
		return settingsDocumentKtav
	}
	return settingsDocumentJSON
}

// decodeSettingsDocument converts raw bytes read from path into a
// settingsDocument. A decode failure is returned as-is: it must never be
// interpreted as a missing field, an absent sync_rev, or a nonexistent
// document, and the codec's error messages carry no document values.
func decodeSettingsDocument(path string, rawBytes []byte, rawFingerprint reloadFileFingerprint) (settingsDocument, error) {
	jsonBytes, err := decodeConfigDocument(path, rawBytes)
	if err != nil {
		return settingsDocument{}, err
	}
	return settingsDocument{
		path:           path,
		jsonBytes:      jsonBytes,
		rawBytes:       rawBytes,
		rawFingerprint: rawFingerprint,
		format:         settingsDocumentFormatOf(path),
	}, nil
}

// readSettingsDocument is THE single choke point for reading any settings or
// protection document. It takes a LOGICAL path in any spelling (legacy rush.json
// or a rush.ktav sibling, any case) and, BY CONSTRUCTION rather than per call
// site:
//
//  1. normalizes it to its logical LEGACY name (legacySlotOf);
//  2. selects the active slot (selectConfigSlot — the Ktav sibling wins);
//  3. fails closed BEFORE reading when the slot is backup-only
//     (checkBackupOnlySlot) — that error propagates as an ERROR and must
//     never be mapped to "no lock"/"unlocked" by any caller;
//  4. stable-reads the SELECTED file and decodes it (decodeConfigDocument);
//     decode errors propagate as errors.
//
// CONTRACT: an os.IsNotExist returned from this function means EXACTLY that
// neither the .ktav nor the .json spelling of the slot exists AND no backup
// exists (a backup-only slot already errored in step 3). Callers may treat
// such an error as "no document"; every other error is a real failure.
func readSettingsDocument(path string) (settingsDocument, error) {
	legacy := legacySlotOf(path)
	selected := selectConfigSlot(legacy)
	if err := checkBackupOnlySlot(legacy); err != nil {
		return settingsDocument{}, err
	}
	rawBytes, rawFingerprint, err := readStableConfigFile(selected)
	if err != nil {
		return settingsDocument{}, err
	}
	return decodeSettingsDocument(selected, rawBytes, rawFingerprint)
}

// encodeSettingsDocument converts JSON-shaped bytes back to the raw bytes a
// format-appropriate commit should write. JSON output is byte-for-byte the
// input, so .json targets behave exactly as before this layer existed. Ktav
// output is produced by the strict JSON-to-Ktav converter, which re-parses
// and refuses anything it cannot represent losslessly.
func encodeSettingsDocument(jsonBytes []byte, format settingsDocumentFormat) ([]byte, error) {
	if format == settingsDocumentJSON {
		return jsonBytes, nil
	}
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("ktav conversion failed: %w", err)
	}
	return ktavBytes, nil
}

// rejectUnconvertedKtavWrite fails a write boundary that has not yet been
// converted to the settings-document layer when its target is a Ktav
// document. Wave B call sites (store_oauth, store_mcp) use this until their
// conversions land.
func rejectUnconvertedKtavWrite(path string) error {
	if isKtavConfigPath(filepath.Clean(path)) {
		return fmt.Errorf("%w: %s", errKtavWriteNotSupported, path)
	}
	return nil
}

// newConfigFileIsKtav reports whether a config file created for a scope that
// currently has NEITHER format on disk should be created as rush.ktav. It is
// gated on ktavAutoMigrate so KTAV-4 flips a single switch.
func newConfigFileIsKtav() bool {
	return ktavAutoMigrate
}

// legacySlotOf returns the LEGACY JSON spelling of a logical config slot:
// a .ktav path maps to its .json sibling, any other path is returned
// unchanged. Mirrors ktavSiblingOf's construction inverted: same directory,
// same base name, .ktav replaced with .json.
//
// The extension strip is deliberately case-insensitive, NOT strings.TrimSuffix:
// isKtavConfigPath matches the extension with strings.EqualFold, so "rush.KTAV"
// is a Ktav path, but TrimSuffix (byte-case-sensitive) would leave it intact
// and produce the bogus slot "rush.KTAV.json". EqualFold-matched extensions
// have identical length, so cutting by length is safe. (The case-insensitive
// classification lives in isKtavConfigPath in ktav_discovery.go, which this
// lane does not own; this helper only keeps the strip consistent with it.)
func legacySlotOf(path string) string {
	if !isKtavConfigPath(path) {
		return path
	}
	return path[:len(path)-len(ktavConfigExt)] + ".json"
}

// configWriteLockAnchor returns the path the inter-process write sidecar
// anchors on: the LEGACY JSON spelling of a .ktav path, so a rush.ktav store
// contends on rush.json.lock — the same sidecar the pre-Ktav write path
// used. A non-Ktav path is returned unchanged, so a .json input anchors on
// itself.
func configWriteLockAnchor(path string) string {
	return legacySlotOf(path)
}

// readSlotOf resolves the physical READ path for a logical slot named by a
// legacy config path: rush.ktav when it is the selected file of the slot,
// otherwise the legacy name. It is the READ counterpart of writeTargetSlot:
// the same guard (empty and already-Ktav inputs pass through unchanged, so
// an already-selected .ktav path is never re-selected into "rush.ktav.ktav"),
// WITHOUT the creation redirect — a read source must observe the slot as it
// exists and never reserve the Ktav spelling for a slot that has neither
// file.
//
// Note: with the slot selection now built into readSettingsDocument, this
// helper is redundant for settings-protection callers but remains in use by
// the platform source helpers (settings_lock_sources*.go) and
// settings_models_integrity.go, which pass its result to path-sensitive
// logic (not just document reads).
func readSlotOf(path string) string {
	if path == "" || isKtavConfigPath(path) {
		return path
	}
	return selectConfigSlot(path)
}

// writeTargetSlot resolves the physical write target for an already-resolved
// legacy config path: rush.ktav when it is the selected file of the slot,
// otherwise the legacy name. A write must never create a legacy rush.json
// when rush.ktav is the active slot. When the slot has NEITHER file, the
// creation decision belongs to newConfigFileIsKtav: only then is the Ktav
// spelling created. An existing legacy file — including a symlinked (even
// dangling) or unreadable rush.json — stays the write target; writes then
// resolve the physical destination through resolveConfigWriteTarget,
// preserving the symlink. That is true only while the symlinked slot is
// pending or deferred (dangling link, unreadable target): a VALID
// symlinked rush.json is migrated to Ktav at Load, after which the active
// slot is rush.ktav and only the symbolic link — never the physical
// target — was removed.
func writeTargetSlot(legacyPath string) string {
	if legacyPath == "" || isKtavConfigPath(legacyPath) {
		return legacyPath
	}
	selected := selectConfigSlot(legacyPath)
	if selected == legacyPath && newConfigFileIsKtav() && !ktavEntryExists(legacyPath) {
		return ktavSiblingOf(legacyPath)
	}
	return selected
}
