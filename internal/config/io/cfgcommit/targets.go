package cfgcommit

import "github.com/PHPCraftdream/rush/internal/config/io/docread"

// configWriteTarget identifies the physical spelling, sidecar lock path, and
// fingerprint a config commit must verify against before publication.
type configWriteTarget struct {
	path         string
	selectedPath string
	lockPath     string
	expected     docread.Fingerprint
	owner        int
	enforce      bool
}
