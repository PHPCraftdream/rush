// Small persisted TUI/option setters (compact mode, transparent background,
// theme, keep-alive) and the clone helpers they use for copy-on-write option
// mutation.
package config

// SetCompactMode sets the compact mode setting and persists it.
func (s *ConfigStore) SetCompactMode(scope Scope, enabled bool) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		tuiCopy := cloneTUIOptions(optsCopy.TUI)
		tuiCopy.CompactMode = enabled
		optsCopy.TUI = tuiCopy
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.tui.compact_mode", enabled)
}

// SetTransparentBackground sets the transparent background setting and persists it.
func (s *ConfigStore) SetTransparentBackground(scope Scope, enabled bool) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		tuiCopy := cloneTUIOptions(optsCopy.TUI)
		tuiCopy.Transparent = &enabled
		optsCopy.TUI = tuiCopy
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.tui.transparent", enabled)
}

// cloneOptions returns a fresh *Options copy suitable for copy-on-write
// mutation: a nil input yields a fresh zero-value Options (matching the
// historical "if s.config.Options == nil { s.config.Options = &Options{} }"
// lazy-init behaviour), and a non-nil input is shallow-copied so the
// caller can freely reassign its own fields (like TUI) without touching
// the Options struct any other snapshot might still be reading.
func cloneOptions(o *Options) *Options {
	if o == nil {
		return &Options{}
	}
	c := *o
	return &c
}

// cloneTUIOptions mirrors cloneOptions for the nested *TUIOptions pointer.
func cloneTUIOptions(t *TUIOptions) *TUIOptions {
	if t == nil {
		return &TUIOptions{}
	}
	c := *t
	return &c
}

// SetTheme sets the TUI theme and persists it.
func (s *ConfigStore) SetTheme(scope Scope, theme string) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		tuiCopy := cloneTUIOptions(optsCopy.TUI)
		tuiCopy.Theme = theme
		optsCopy.TUI = tuiCopy
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.tui.theme", theme)
}

// SetKeepAliveEnabled persists the WebAudio keep-alive preference.
// Persisted as a literal bool (NOT *bool) so the JSON form is
// `"keep_alive_enabled": true|false` — the in-memory Options carries a
// *bool only to distinguish "not set, use default ON" from an explicit
// choice, and SetConfigField writes the underlying primitive.
func (s *ConfigStore) SetKeepAliveEnabled(scope Scope, enabled bool) error {
	s.updateConfig(func(cfgCopy *Config) {
		optsCopy := cloneOptions(cfgCopy.Options)
		v := enabled
		optsCopy.KeepAliveEnabled = &v
		cfgCopy.Options = optsCopy
	})
	return s.SetConfigField(scope, "options.keep_alive_enabled", enabled)
}
