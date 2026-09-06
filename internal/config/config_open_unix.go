//go:build !windows

package config

import "os"

func openStableConfigFile(path string) (*os.File, error) {
	return os.Open(path)
}

func writeStableConfigFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
