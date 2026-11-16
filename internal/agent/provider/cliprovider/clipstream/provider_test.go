package clipstream

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/clipspec"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

// fastExitHelperEnv turns this test binary into the fast-exiting child process
// used by TestStreamFastExitNoLastLineLoss. See that test for why the child is
// the test binary rather than a shell.
const fastExitHelperEnv = "RUSH_TEST_FAST_EXIT_FILE"

func TestMain(m *testing.M) {
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testguard: isolate global state: %v\n", err)
		os.Exit(1)
	}
	// Child-process mode: dump the fixture and exit before the testing
	// framework emits anything of its own (no flag parsing, no "PASS" line),
	// so the parent's scanner sees exactly the bytes of the file.
	if path := os.Getenv(fastExitHelperEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fast-exit helper:", err)
			os.Exit(1)
		}
		os.Stdout.Write(data)
		os.Exit(0)
	}
	// go-pty's Windows ConPTY path has an internal data race that the -race
	// detector flags. Force pipe mode for the whole cliprovider suite on
	// Windows so streaming tests stay race-clean; Unix keeps PTY coverage.
	testDisablePTY = runtime.GOOS == "windows"
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestBuildArgsPromptFlag(t *testing.T) {
	for _, spec := range clipspec.All {
		args := spec.BuildArgs(false)
		for _, arg := range args {
			if arg == spec.PromptFlag {
				t.Errorf("BuildArgs for %s should not contain prompt flag %q", spec.ModelID, spec.PromptFlag)
			}
		}
	}
}

func TestAllSpecsHavePartParser(t *testing.T) {
	for _, spec := range clipspec.All {
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", spec.ModelID)
		}
	}
}

// ── Spec invariants ──────────────────────────────────────────────────────────

func TestAllSpecsHaveUniqueIDs(t *testing.T) {
	seen := make(map[string]bool)
	for _, spec := range clipspec.All {
		if seen[spec.ModelID] {
			t.Errorf("duplicate ModelID: %q", spec.ModelID)
		}
		seen[spec.ModelID] = true
	}
}

func TestCodexSpecsHaveAlwaysStdin(t *testing.T) {
	for _, spec := range clipspec.All {
		if spec.Binary == "codex" && !spec.AlwaysStdin {
			t.Errorf("codex spec %q must have AlwaysStdin=true", spec.ModelID)
		}
	}
}

func TestQwenSpecHasAlwaysStdin(t *testing.T) {
	for _, spec := range clipspec.All {
		if spec.Binary == "qwen" && !spec.AlwaysStdin {
			t.Errorf("qwen spec %q must have AlwaysStdin=true", spec.ModelID)
		}
	}
}

func TestCodexSpecsHaveCorrectBinary(t *testing.T) {
	codexIDs := []string{
		"cli-codex",
		"cli-codex-gpt-5-4",
		"cli-codex-gpt-5-2",
		"cli-codex-max",
		"cli-codex-gpt-5-2-base",
		"cli-codex-mini",
	}
	specsByID := make(map[string]CLISpec)
	for _, s := range clipspec.All {
		specsByID[s.ModelID] = s
	}
	for _, id := range codexIDs {
		spec, ok := specsByID[id]
		if !ok {
			t.Errorf("missing expected codex spec %q", id)
			continue
		}
		if spec.Binary != "codex" {
			t.Errorf("spec %q has Binary=%q, want 'codex'", id, spec.Binary)
		}
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", id)
		}
		if spec.ParseUsageLine == nil {
			t.Errorf("spec %q has nil ParseUsageLine", id)
		}
	}
}

func TestAll_HaikuModelsRegistered(t *testing.T) {
	// After the 2026-06-17 cleanup the per-thinking and npx variants
	// were removed. We only carry the canonical `cli-claude-haiku`
	// alias now; the operator picks effort via the UI selector and the
	// cliprovider forwards it through context at call time.
	want := []string{
		"cli-claude-haiku",
	}
	byID := make(map[string]CLISpec, len(clipspec.All))
	for _, s := range clipspec.All {
		byID[s.ModelID] = s
	}
	for _, id := range want {
		spec, ok := byID[id]
		if !ok {
			t.Errorf("missing expected spec %q in All", id)
			continue
		}
		if spec.ContextWindow != 200_000 {
			t.Errorf("spec %q ContextWindow = %d, want 200_000", id, spec.ContextWindow)
		}
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", id)
		}
		if spec.ParseUsageLine == nil {
			t.Errorf("spec %q has nil ParseUsageLine", id)
		}
	}
}
