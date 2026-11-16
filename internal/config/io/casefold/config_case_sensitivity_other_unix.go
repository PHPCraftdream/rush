//go:build !windows && !darwin && !linux && !freebsd

package casefold

func ConfigPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return ConfigFoldCaseLeaf(leaf, ConfigPlatformCaseSensitivity(dir))
}

func ConfigPlatformCaseSensitivity(dir string) ConfigCaseSensitivity {
	return DirectoryCaseSensitivityByEntries(dir)
}
