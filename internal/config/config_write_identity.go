package config

import (
	"fmt"
	"path/filepath"
)

// configWriteTargetDedupKey identifies the physical object protected by a
// target's sidecar. Existing config files use their descriptor identity.
// Missing files use the sidecar identity when available, then the parent
// identity and a case-aware leaf key.
func configWriteTargetDedupKey(target configWriteTarget) string {
	if target.expected.exists && target.expected.identity.valid {
		return configWriteIdentityKey("file", target.expected.identity)
	}
	if identity, err := configFileIdentityAtPath(target.lockPath); err == nil && identity.valid {
		return configWriteIdentityKey("sidecar", identity)
	}

	parent := target.expected.parentIdentity
	if !parent.valid {
		parent = configParentIdentity(target.selectedPath)
	}
	if !parent.valid {
		return "missing-path:" + normalizeDiscoveryPath(target.path)
	}

	leaf := filepath.Base(target.path)
	leaf, _ = configPlatformCaseFoldLeaf(filepath.Dir(target.path), leaf)
	if leaf == "" {
		leaf = filepath.Base(target.path)
	}
	return fmt.Sprintf("missing-parent:%d:%d:%s", parent.device, parent.inode, leaf)
}

func configWriteIdentityKey(kind string, identity configFileIdentity) string {
	return fmt.Sprintf("%s:%d:%d", kind, identity.device, identity.inode)
}

func configDirectoryIsCaseInsensitive(dir string) bool {
	_, ok := configPlatformCaseFoldLeaf(dir, "Aa")
	return ok
}

func configEquivalentLeafNames(dir, left, right string) bool {
	leftKey, leftFold := configPlatformCaseFoldLeaf(dir, left)
	rightKey, rightFold := configPlatformCaseFoldLeaf(dir, right)
	return leftFold && rightFold && leftKey == rightKey
}
