//go:build !windows && !darwin && !linux && !freebsd

package config

func configPlatformCaseFoldLeaf(dir, leaf string) (string, bool) {
	return configFoldCaseLeaf(leaf, configPlatformCaseSensitivity(dir))
}

func configPlatformCaseSensitivity(dir string) configCaseSensitivity {
	return configDirectoryCaseSensitivityByEntries(dir)
}
