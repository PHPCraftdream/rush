package provresolve

// discoveryEnabled reports whether provider id is inside the discovery
// scope. A nil scope means every provider — the behavior is unchanged
// when no scope was configured at Load time.
func discoveryEnabled(scope map[string]bool, id string) bool {
	return scope == nil || scope[id]
}
