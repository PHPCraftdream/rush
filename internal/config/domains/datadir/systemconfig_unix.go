//go:build !windows

package datadir

// systemConfigPath is the system-wide configuration file path. It is
// loaded at the lowest priority so user and project configs override it.
const systemConfigPath = "/etc/rush/rush.json"
