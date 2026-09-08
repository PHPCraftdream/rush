//go:build !windows && !darwin

package config

func configPlatformCaseFoldLeaf(_, _ string) (string, bool) {
	return "", false
}
