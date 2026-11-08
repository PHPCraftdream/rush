package config

// SetBeforeConfigCommitForTest открывает существующий pre-commit seam для межпакетных tests.
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
