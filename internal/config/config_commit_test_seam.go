package config

// SetBeforeConfigCommitForTest exposes the existing seam to cross-package tests.
func SetBeforeConfigCommitForTest(hook func()) func() {
	configTestHooks.Lock()
	previous := configTestHooks.beforeCommitCheck
	configTestHooks.beforeCommitCheck = hook
	configTestHooks.Unlock()
	return func() {
		configTestHooks.Lock()
		configTestHooks.beforeCommitCheck = previous
		configTestHooks.Unlock()
	}
}
