//go:build windows

package config

// Windows does not expose Unix uid ownership. fsext.Owner returns -1 there,
// which is the deliberate platform-neutral bypass for this policy.
func homeConfigOwner() int { return -1 }

func systemConfigOwner() int { return -1 }
