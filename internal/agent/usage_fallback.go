package agent

// approxTokenCount is the shared rough token estimator (kept in the root
// package; the usage package receives it injected via fallbackStepUsage).
func approxTokenCount(s string) int64 {
	if s == "" {
		return 0
	}
	return int64((len(s) + 3) / 4)
}
