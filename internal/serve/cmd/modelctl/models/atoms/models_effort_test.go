package modelatoms

// setRegistryEffortLevelsForTesting creates a mock effort source that returns fixed levels
// without shelling out. Returns a restore function.
func setRegistryEffortLevelsForTesting(levels []string) func() {
	mock := &cliEffortSource{
		Binary:  "__test__",
		HelpArg: "--help",
		Regex:   `^$`,
	}
	effortCacheMu.Lock()
	effortCache["__test__"] = levels
	effortCacheMu.Unlock()
	old := *claudeEffortSource
	*claudeEffortSource = *mock
	return func() {
		*claudeEffortSource = old
		resetEffortCache()
	}
}
