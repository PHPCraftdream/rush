//go:build !windows

package config

import "os"

// homeConfigOwner is the explicit trust policy for user-level config files.
// It must not inherit the owner of the checkout that happens to be loaded.
func homeConfigOwner() int { return os.Getuid() }

// systemConfigOwner is the explicit trust policy for the system config.
func systemConfigOwner() int { return 0 }
