package config

// anySettingsLockExists reports whether any settings lock file is present.
// A read error counts as "lock exists" and fails closed. The check itself
// lives in the settings component (settings.Store.AnyLockExists).
func (s *ConfigStore) anySettingsLockExists() bool {
	return s.settings.AnyLockExists()
}

// verifyModelsIntegrityAtLoad performs the one-time startup check. It never
// fails Load and never writes anything; it only records the result.
func (s *ConfigStore) verifyModelsIntegrityAtLoad() {
	s.settings.VerifyModelsIntegrityAtLoad()
}

// ModelsIntegrity returns the one-time startup check result. It is computed
// only during Load and never recomputed by reload.
func (s *ConfigStore) ModelsIntegrity() error {
	return s.settings.ModelsIntegrity()
}
