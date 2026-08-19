//go:build !windows && !linux

package session

// startTimeToken has no implementation on this platform (macOS, BSD, ...):
// there is no /proc filesystem, and reading a process's start time
// portably would require a sysctl(KERN_PROC_PID)/libproc cgo binding this
// codebase does not otherwise depend on. Always reports unavailable, which
// verifyGroupStillPlausible treats as "fall back to the group-leader check
// alone" -- see its doc comment for the accepted, narrower residual TOCTOU
// window this leaves on these platforms.
func startTimeToken(pid int) (string, bool) {
	return "", false
}
