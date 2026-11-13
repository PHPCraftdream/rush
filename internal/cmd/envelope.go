// Fork-only file: resolves and loads run-result envelopes
// (.rush/stdin/<session>.out) shared by `rush result` and
// `rush run --findings-from`. Needs no database, session store or
// network: a session id is located via the main checkout found with
// `git rev-parse --git-common-dir`, so it also works from inside a
// linked worktree.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/platform"
)

// resolveEnvelopePath turns a reference (session id or file path) into
// an envelope file path. A direct path wins; otherwise the id is looked
// up as <main-checkout>/.rush/stdin/<id>.out.
func resolveEnvelopePath(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty envelope reference")
	}
	if st, err := os.Stat(ref); err == nil && !st.IsDir() {
		p, absErr := filepath.Abs(ref)
		if absErr != nil {
			p = filepath.Clean(ref)
		}
		return p, nil
	}
	idPath := filepath.Join(".rush", "stdin", ref+".out")
	mainRoot, err := envelopeMainCheckoutRoot()
	if err != nil {
		return "", fmt.Errorf("no envelope for session %q; resolving it needs the main checkout, which failed: %w (pass a direct path to the envelope file instead)", ref, err)
	}
	path := filepath.Join(mainRoot, idPath)
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		p, absErr := filepath.Abs(path)
		if absErr != nil {
			p = filepath.Clean(path)
		}
		return p, nil
	}
	return "", fmt.Errorf("no envelope for session %q (looked for %s) and %q is not a readable file", ref, path, ref)
}

// envelopeMainCheckoutRoot returns the main checkout's root directory, derived
// from the git common dir. Works from inside a linked worktree too.
func envelopeMainCheckoutRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine the working directory: %w", err)
	}
	cmd := platform.Command(context.Background(), "git", "rev-parse", "--git-common-dir")
	cmd.Dir = dir
	stripWorktreeEnv(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("cannot locate the main checkout (git rev-parse --git-common-dir failed: %w); pass an explicit envelope file path", err)
	}
	return mainRootFromCommonDirOutput(dir, string(out))
}

// mainRootFromCommonDirOutput converts `git rev-parse --git-common-dir`
// output into the main checkout root. In the main checkout git prints
// ".git" (relative), so the main root is the working directory itself;
// in a linked worktree it prints the absolute <main>/.git, so the main
// root is <main>.
func mainRootFromCommonDirOutput(dir, out string) (string, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return "", fmt.Errorf("empty git rev-parse output")
	}
	p := filepath.FromSlash(trimmed)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	if filepath.Base(p) != ".git" {
		return "", fmt.Errorf("git common dir is %s; cannot derive the main checkout root from it; pass an explicit envelope file path", p)
	}
	return filepath.Dir(p), nil
}

// stripWorktreeEnv removes the GIT_* variables git exports to hooks so
// the probe observes the real repository, not the hook's view. Local
// copy of the unexported helper in internal/config/datadir.go.
func stripWorktreeEnv(cmd *exec.Cmd) {
	stripped := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true,
		"GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true,
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if i := strings.IndexByte(entry, '='); i > 0 && stripped[entry[:i]] {
			continue
		}
		filtered = append(filtered, entry)
	}
	cmd.Env = filtered
}

// loadEnvelope reads an envelope file and parses it both as a generic
// JSON object (for --field) and as the typed app.RunResult. The raw
// bytes are returned so `rush result --json` can write them verbatim.
func loadEnvelope(path string) ([]byte, *app.RunResult, map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("cannot read envelope %s: %w", path, err)
	}
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid envelope JSON in %s: %w", path, err)
	}
	m, ok := probe.(map[string]any)
	if !ok {
		return nil, nil, nil, fmt.Errorf("envelope %s is not a JSON object", path)
	}
	env := &app.RunResult{}
	if err := json.Unmarshal(data, env); err != nil {
		return nil, nil, nil, fmt.Errorf("invalid envelope JSON in %s: %w", path, err)
	}
	return data, env, m, nil
}
