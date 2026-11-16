package cfgcommit

import "github.com/PHPCraftdream/rush/internal/config/io/cfghooks"

// SetBeforeConfigCommitForTest exposes the existing seam to cross-package
// tests through the cfghooks provider bridge.
func SetBeforeConfigCommitForTest(hook func()) func() {
	return cfghooks.SetBeforeCommitCheckForTest(hook)
}
