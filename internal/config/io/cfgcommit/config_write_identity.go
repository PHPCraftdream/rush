package cfgcommit

import (
	"fmt"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config/io/casefold"
	"github.com/PHPCraftdream/rush/internal/config/io/docread"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
)

// configWriteTargetDedupKey identifies the physical object protected by a
// target's sidecar. Existing config files use their descriptor identity.
// Missing files use the sidecar identity when available, then the parent
// identity and a case-aware leaf key.
func configWriteTargetDedupKey(target configWriteTarget) string {
	return configWriteTargetDedupKeyWithCaseFold(target, casefold.ConfigPlatformCaseFoldLeaf)
}

// ConfigWriteTargetDedupKeyWithCaseFold is the exported form of
// configWriteTargetDedupKeyWithCaseFold for the root test forwarders.
func ConfigWriteTargetDedupKeyWithCaseFold(target configWriteTarget, caseFold func(string, string) (string, bool)) string {
	return configWriteTargetDedupKeyWithCaseFold(target, caseFold)
}

func configWriteTargetDedupKeyWithCaseFold(target configWriteTarget, caseFold func(string, string) (string, bool)) string {
	if target.expected.Exists && target.expected.Identity.Valid {
		return configWriteIdentityKey("file", target.expected.Identity)
	}
	if identity, err := fileid.FileIdentityAtPath(target.lockPath); err == nil && identity.Valid {
		return configWriteIdentityKey("sidecar", identity)
	}

	parent := target.expected.ParentIdentity
	if !parent.Valid {
		parent = fileid.ConfigParentIdentity(target.selectedPath)
	}
	if !parent.Valid {
		return "missing-path:" + docread.NormalizeDiscoveryPath(target.path)
	}

	leaf := filepath.Base(target.path)
	if caseFold != nil {
		leaf, _ = caseFold(filepath.Dir(target.path), leaf)
	}
	if leaf == "" {
		leaf = filepath.Base(target.path)
	}
	return fmt.Sprintf("missing-parent:%d:%d:%s", parent.Device, parent.Inode, leaf)
}

func configWriteTargetDedupKeyWithCaseSensitivity(target configWriteTarget, sensitivity casefold.ConfigCaseSensitivity) string {
	return configWriteTargetDedupKeyWithCaseFold(target, func(_, leaf string) (string, bool) {
		return casefold.ConfigFoldCaseLeaf(leaf, sensitivity)
	})
}

func configWriteIdentityKey(kind string, identity fileid.FileIdentity) string {
	return fmt.Sprintf("%s:%d:%d", kind, identity.Device, identity.Inode)
}

func configDirectoryIsCaseInsensitive(dir string) bool {
	_, ok := casefold.ConfigPlatformCaseFoldLeaf(dir, "Aa")
	return ok
}

func configEquivalentLeafNames(dir, left, right string) bool {
	leftKey, leftFold := casefold.ConfigPlatformCaseFoldLeaf(dir, left)
	rightKey, rightFold := casefold.ConfigPlatformCaseFoldLeaf(dir, right)
	return leftFold && rightFold && leftKey == rightKey
}

// ConfigWriteTargetDedupKey exposes the target dedup key to the root package
// (io layer that will consume it directly in a later pass: root).
func ConfigWriteTargetDedupKey(target configWriteTarget) string {
	return configWriteTargetDedupKey(target)
}

// ConfigWriteTargetDedupKeyWithCaseSensitivity exposes the case-sensitivity
// aware dedup key to the root package (io layer: root).
func ConfigWriteTargetDedupKeyWithCaseSensitivity(target configWriteTarget, sensitivity casefold.ConfigCaseSensitivity) string {
	return configWriteTargetDedupKeyWithCaseSensitivity(target, sensitivity)
}

// ConfigDirectoryIsCaseInsensitive exposes the platform case-insensitivity
// probe to the root package (io layer: root).
func ConfigDirectoryIsCaseInsensitive(dir string) bool {
	return configDirectoryIsCaseInsensitive(dir)
}

// ConfigEquivalentLeafNames exposes the case-equivalence leaf comparison to
// the root package (io layer: root).
func ConfigEquivalentLeafNames(dir, left, right string) bool {
	return configEquivalentLeafNames(dir, left, right)
}

// NewConfigWriteTarget builds a cfgcommit write target from public fields so
// the root package can delegate dedup-key computation (io layer: root).
func NewConfigWriteTarget(path, selectedPath, lockPath string, expected docread.Fingerprint, owner int, enforce bool) configWriteTarget {
	return configWriteTarget{
		path:         path,
		selectedPath: selectedPath,
		lockPath:     lockPath,
		expected:     expected,
		owner:        owner,
		enforce:      enforce,
	}
}
