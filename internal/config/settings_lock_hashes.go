package config

// settingsLockSource identifies a settings file and its authorization scope.
type settingsLockSource struct {
	path   string
	global bool
}

// collectSettingsSourceHashes reads every configured lock source and preserves
// every non-empty hash so no source can silently override another.
func collectSettingsSourceHashes(sources []settingsLockSource) ([]string, []string, error) {
	globals := make([]string, 0, len(sources))
	locals := make([]string, 0, len(sources))
	for _, source := range sources {
		if source.path == "" {
			continue
		}
		hash, err := freshSettingsHash(source.path)
		if err != nil {
			return nil, nil, err
		}
		if hash == "" {
			continue
		}
		if source.global {
			globals = append(globals, hash)
		} else {
			locals = append(locals, hash)
		}
	}
	return globals, locals, nil
}
