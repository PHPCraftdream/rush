//go:build !windows && !darwin

package config

import "github.com/PHPCraftdream/rush/internal/config/io/fsx/casefold"

// io layer: casefold. Behavioral directory scan exists only off windows/darwin.
func configDirectoryCaseSensitivityByEntries(dir string) configCaseSensitivity {
	return casefold.DirectoryCaseSensitivityByEntries(dir)
}
