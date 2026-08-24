// Local availability detection: which CLI binaries are reachable on
// PATH (with a PATH/PATHEXT-keyed memoization cache), plus the
// WSL-aware binary resolver both launch paths in Stream rely on.

package cliprovider

import (
	"os"
	"slices"
	"sync"

	"github.com/PHPCraftdream/rush/internal/shell"
)

// Available returns the subset of All whose Binary is found in PATH.
// AvailableFunc returns the locally-installed CLI specs. It is a package
// var so tests can stub CLI detection deterministically — otherwise the set
// depends on whatever binaries (claude, gemini, npx, …) happen to be on the
// runner's PATH, which makes provider-count assertions environment-dependent.
var AvailableFunc = detectAvailable

// testDisablePTY, when true, forces pipe mode regardless of spec. The
// cliprovider test suite sets it on Windows, where go-pty's ConPTY path has
// an internal data race the -race detector flags. Always false in production.
var testDisablePTY bool

// resolveBinary resolves name (a bare command like "claude" or "bash") to
// the executable that will actually run, going through the same
// WSL-launcher-aware PATH lookup internal/shell uses for #!/bin/bash
// shebang dispatch (see shell.LookPathSkippingWSL).
//
// This matters specifically for Binary: "bash": plain exec.LookPath (and
// the implicit lookup os/exec.Cmd performs on a bare name) can resolve to
// %SystemRoot%\System32\bash.exe — the WSL launcher — if it happens to sit
// ahead of Git Bash/MSYS bash on PATH. The WSL launcher expects Linux-style
// paths (/mnt/c/...) and cannot run a script given a Windows-style path or
// working directory, so silently handing it back here would launch a
// process that appears to start but fails in confusing ways downstream.
// Both call sites in Stream (PTY branch and pipe-fallback branch) resolve
// through this helper rather than trusting exec.LookPath/os/exec's default
// resolution directly.
//
// On non-Windows this is a thin wrapper around exec.LookPath.
func resolveBinary(name string) (string, error) {
	return shell.LookPathSkippingWSL(name)
}

// Available reports which CLI model specs are usable on this machine.
func Available() []CLISpec { return AvailableFunc() }

// detectAvailableCache memoizes detectAvailable's PATH scan, keyed on the
// PATH (and, on Windows, PATHEXT) value the scan was performed against.
//
// Why this cache exists: detectAvailable resolves one binary per distinct
// CLISpec.Binary (claude/gemini/codex/qwen) through exec.LookPath, and
// exec.LookPath does no caching of its own — every call re-walks every PATH
// entry, on Windows multiplied by every PATHEXT suffix. Measured on a
// developer workstation with 71 PATH entries and 11 PATHEXT suffixes, one
// detectAvailable() call costs ~360ms (~90ms per binary); a runner where
// none of the four CLIs are installed pays the full unsuccessful walk for
// each.
//
// That would be a once-per-process cost if Available() were called once, but
// it is called from config.configureProviders, which runs on the initial
// config Load AND on every buildAndPublishReload — i.e. once per config
// field write. A single `crush models use ...` invocation triggers ~5 of
// them, so ~1.8s of the command's runtime was spent re-answering "is
// claude on PATH?" with an answer that had not changed. In the test suite
// the same multiplier applied per test: internal/cmd's models_* tests each
// paid ~2s of PATH scanning out of a ~2.8s total runtime.
//
// Keying on PATH/PATHEXT rather than caching unconditionally keeps the
// tests that install a fake binary into a temp dir and point PATH at it
// (internal/agent/cliprovider/provider_windows_test.go, internal/shell's
// dispatch tests) working unchanged: changing PATH invalidates the entry.
// The remaining behavioral difference from an uncached scan is that
// installing a CLI into an already-on-PATH directory while a long-lived
// crush process is running is no longer picked up by a config reload; it
// needs a restart. That is the same trade the fork already makes for the
// Catwalk/Hyper provider catalogs (see internal/config/catwalk.go's
// providerCacheTTL) and is why detection is only ever consulted to
// synthesize the local-cli provider list, never on the hot path of actually
// launching a CLI (Stream resolves its binary directly through
// resolveBinary every time).
var detectAvailableCache struct {
	mu     sync.Mutex
	key    string
	specs  []CLISpec
	loaded bool
}

// detectAvailablePathKey returns the cache key identifying the lookup
// environment detectAvailable would resolve against. PATHEXT only affects
// resolution on Windows, but including it unconditionally costs nothing and
// keeps the key definition platform-independent.
func detectAvailablePathKey() string {
	return os.Getenv("PATH") + "\x00" + os.Getenv("PATHEXT")
}

func detectAvailable() []CLISpec {
	key := detectAvailablePathKey()

	detectAvailableCache.mu.Lock()
	defer detectAvailableCache.mu.Unlock()
	if detectAvailableCache.loaded && detectAvailableCache.key == key {
		// Hand back a copy: callers (config.configureProviders) range over
		// the result and a shared backing array would let one caller's
		// append alias another's.
		return slices.Clone(detectAvailableCache.specs)
	}

	seen := make(map[string]bool)
	var result []CLISpec
	for _, spec := range All {
		if !seen[spec.Binary] {
			_, err := resolveBinary(spec.Binary)
			seen[spec.Binary] = err == nil
		}
		if seen[spec.Binary] {
			result = append(result, spec)
		}
	}

	detectAvailableCache.key = key
	detectAvailableCache.specs = result
	detectAvailableCache.loaded = true
	return slices.Clone(result)
}
