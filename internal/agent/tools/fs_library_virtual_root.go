package tools

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// LibraryVirtualRoot is the canonical definition backing sdk.LibraryVirtualRoot
// (see that doc comment for the full history/rationale). Defined here, not
// in package sdk, because this package cannot import sdk — sdk already
// imports this package for DiskProvider/OSDisk — but a fs_* tool call
// resolving a path against the REAL disk must recognize this sentinel to
// refuse it (R6-1, P0 SDK review round 6: an OSDisk-backed tool must never
// operate against the synthetic ephemeral-library-mode namespace, no
// matter how a call reaches that far).
//
// Computed per-OS, unlike the single fixed literal a prior round (R5-6)
// used, so the value itself satisfies the REAL, platform-native
// filepath.IsAbs -- not merely filepathext.SmartIsAbs's cross-platform
// "absolute enough to skip re-rooting via filepath.Abs" notion. Before
// this fix, the one literal "/rush-library-mode-root" was genuinely
// absolute on Unix but only filepathext.SmartIsAbs (a driveless leading
// "/") on Windows -- violating DiskProvider's own documented contract
// that "every method receives an absolute path"
// (internal/agent/tools/fs_provider.go): a strict custom DiskProvider
// checking the real filepath.IsAbs could correctly reject a path the SDK
// itself produced (R6-1 finding).
//
// The Windows drive letter chosen is deliberately not one of the
// conventional ones (C, D) reserved for a real system/data volume, but
// no single-letter choice can be guaranteed collision-free against every
// possible real mapped drive on every machine. That no longer matters
// for safety: rejectRealDiskUnderLibraryVirtualRoot below refuses every
// REAL OSDisk operation resolving under this sentinel unconditionally,
// so even a machine where this exact drive letter happens to be a real,
// live volume never has it touched through this path -- the sentinel
// denies/fails closed rather than granting anything, exactly like the
// prior round's literal was already documented (but not yet enforced) to
// do.
var LibraryVirtualRoot = libraryVirtualRootForOS()

func libraryVirtualRootForOS() string {
	if runtime.GOOS == "windows" {
		return `K:\rush-library-mode-root`
	}
	return "/rush-library-mode-root"
}

// rejectRealDiskUnderLibraryVirtualRoot fails closed (returns an error,
// never a silently-substituted path) when a REAL OSDisk operation would
// resolve inside the ephemeral library-mode sentinel namespace.
// LibraryVirtualRoot is a synthesized ConfigStore.WorkingDir() placeholder
// for a session with no real working directory (sdk.ModeLibrary with no
// Options.WorkingDir) -- not an actual sandboxed root -- so the real
// filesystem has no business ever being consulted under it.
//
// This only ever fires when disk is the real filesystem (diskOrOS's nil
// fallback, or a caller explicitly handing back OSDisk()); a caller's own
// custom DiskProvider is untouched here, exactly like every other
// real-disk-specific behaviour in this package (see DiskProvider's "not a
// security boundary" doc comment) -- a custom provider's own paths, in
// whatever namespace the caller chose, are the caller's business.
//
// resolved must already be produced by resolveScopedPath's join+clean
// step (native separators, not yet symlink-walked) so the comparison
// happens BEFORE the first real disk.Stat call this function's caller is
// about to make -- the point of failing closed here is that zero real
// syscalls are issued for a sentinel-rooted path, not merely that the
// eventual result is denied.
func rejectRealDiskUnderLibraryVirtualRoot(disk DiskProvider, resolved string) error {
	if disk != OSDisk() {
		return nil
	}
	slashed := filepath.ToSlash(resolved)
	sentinel := filepath.ToSlash(LibraryVirtualRoot)
	if slashed != sentinel && !strings.HasPrefix(slashed, sentinel+"/") {
		return nil
	}
	return fmt.Errorf(
		"fs: refusing real-disk access under the ephemeral library-mode sentinel root %q: "+
			"this session has no real working directory (sdk.ModeLibrary with no WorkingDir) -- "+
			"supply your own DiskProvider (with a FolderScope) to enable file tools for it",
		LibraryVirtualRoot)
}
