// Package-level contract for trusted settings writes: the per-call
// authority value, its meaning, and the audit source label it maps to.
package config

// SettingsWriteAuthority identifies who is asking for a settings write and
// therefore which vetoes apply to that specific call. It is an explicit
// trailing parameter on the *WithAuthority write methods, never ambient
// state: there is no process-wide default to flip, no context key, and no
// way for a JSON field, header, source address, or transport to widen it.
// The zero value is the guarded one, so every caller that does not
// deliberately present an authority inherits today's behavior.
type SettingsWriteAuthority int

const (
	// SettingsAuthorityGuarded is the default authority. The settings lock
	// vetoes protected writes exactly as it did before this type existed.
	SettingsAuthorityGuarded SettingsWriteAuthority = iota
	// SettingsAuthorityTrustedUI is presented only by writes that arrived
	// through the web server's UI-authenticated listener transport (the
	// uiAuth listener whose Auth.Middleware verified the session
	// cookie/token before the WebSocket was accepted). It lifts ONLY the
	// password/settings-lock veto for the single write call that carries
	// it. Reserved keys (sync_rev, cache_rev), model-integrity signing,
	// CAS/atomic commit, scope ownership, interprocess locks, and reload
	// semantics are all unaffected.
	SettingsAuthorityTrustedUI
)

// settingsAuditSource maps an authority to the SettingsEvent.Source label.
// The label is a fixed, secret-free token: it identifies the transport the
// write arrived on, never the person or their credentials.
func settingsAuditSource(authority SettingsWriteAuthority) string {
	if authority == SettingsAuthorityTrustedUI {
		return "ui"
	}
	return ""
}

// liftsSettingsLock reports whether this authority may bypass the
// password/settings-lock veto. Only the trusted-UI authority does.
func (a SettingsWriteAuthority) liftsSettingsLock() bool {
	return a == SettingsAuthorityTrustedUI
}
