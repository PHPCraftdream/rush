//go:build !windows && !linux

package session

// startTimeToken has no implementation on this platform (macOS, BSD, ...):
// there is no /proc filesystem, and reading a process's start time
// portably would require a sysctl(KERN_PROC_PID)/libproc cgo binding this
// codebase does not otherwise depend on. Always reports unavailable, which
// keeps the leader branch of verifyGroupStillPlausibleOutcome on its
// group-leader-check-only fallback -- and, since groupMemberPIDs has no
// implementation either, RegisterChildGroup can record no member witnesses,
// so a registered group whose leader has been reaped can no longer be
// proven historically ours at sweep time and is left RETAINED, unsignalled
// (the conservative "inconclusive, do not kill" direction) -- see
// verifyGroupStillPlausibleOutcome's leader-gone branch.
func startTimeToken(pid int) (string, bool) {
	return "", false
}

// groupMemberPIDs has no implementation on this platform: there is no
// /proc filesystem, and enumerating a process group's members portably
// would require sysctl(KERN_PROC_ALL)/libproc cgo bindings this codebase
// does not otherwise depend on. Always reports no members, which
// captureGroupWitnesses turns into an empty witness list and the sweep
// treats as "identity unverifiable -- do not kill".
func groupMemberPIDs(pgid int) []int {
	_ = pgid
	return nil
}
