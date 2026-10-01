package agentguard

import (
	"os"
	"path/filepath"
	"strings"
)

// gitWriteSubcommands is the denylist of git subcommands that mutate
// repository state (refs, index, working tree, remotes). The orchestrator
// owns commits in delegated runs; an agent run inside a linked worktree
// must not reach for them.
var gitWriteSubcommands = map[string]bool{
	"add":         true,
	"am":          true,
	"apply":       true,
	"cherry-pick": true,
	"checkout":    true,
	"clean":       true,
	"commit":      true,
	"merge":       true,
	"mv":          true,
	"pull":        true,
	"push":        true,
	"rebase":      true,
	"reset":       true,
	"restore":     true,
	"rm":          true,
	"stash":       true,
	"worktree":    true,
}

// gitBranchDeleteFlags are the branch subcommand's deletion flags; plain
// `git branch <name>` (creating a branch) is not in scope for this guard.
var gitBranchDeleteFlags = map[string]bool{
	"-d":       true,
	"-D":       true,
	"--delete": true,
}

// GitWriteError is returned by CheckGitWrites when a command would mutate
// git state in a run where the orchestrator owns commits.
type GitWriteError struct {
	Subcommand string // the matched git subcommand ("checkout", "stash", …)
	Snippet    string // the offending segment, for forensic context
}

func (e *GitWriteError) Error() string {
	return "git writes are not allowed for this run; the orchestrator commits " +
		"(git " + e.Subcommand + " in: " + e.Snippet + ")"
}

// CheckGitWrites inspects a shell command string for git state-mutating
// invocations and returns *GitWriteError for the first one. Read-only git
// (status/diff/log/show/rev-parse/ls-files/blame/grep, plain `git branch`,
// …) passes. Reuses the same segment-splitting, wrapper-stripping and
// shell-runner recursion as Check, so `git reset --hard` behind `bash -c`
// or `env` is still caught.
func CheckGitWrites(command string) error {
	if command == "" {
		return nil
	}
	for _, segment := range splitChained(command) {
		if err := checkSegmentGitWrites(segment); err != nil {
			return err
		}
	}
	return nil
}

func checkSegmentGitWrites(segment string) error {
	return checkGitWritesTokens(tokenize(segment), segment)
}

// CheckGitWritesArgs is CheckGitWrites's counterpart for structured argv:
// same denylist, same wrapper/shell-runner recursion, but the head is
// resolved without re-tokenizing, so argument boundaries survive. The
// first argument naming a shell runner or command wrapper recurses into
// the string CheckGitWrites expects.
func CheckGitWritesArgs(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	return checkGitWritesTokens(argv, strings.Join(argv, " "))
}

// checkGitWritesTokens is the shared core of both entry points: the string
// path hands it a tokenized segment, the argv path the already-split
// arguments. snippet is only used for the error's forensic context.
func checkGitWritesTokens(tokens []string, snippet string) error {
	if len(tokens) == 0 {
		return nil
	}
	res := resolveCommandHead(tokens)
	if res.headCanon == "" {
		return nil
	}
	if res.headCanon == "git" {
		if sub := gitSubcommand(res.rest); sub != "" {
			return gitWriteDecision(sub, res.rest, snippet)
		}
		return nil
	}
	// Recurse one level into shell runners and command wrappers the same
	// way Check does, so a write hidden inside `bash -c "git reset …"`
	// is still caught.
	if shellRunners[res.headCanon] {
		if inner := extractShellInner(res.headCanon, res.rest); inner != "" {
			return CheckGitWrites(inner)
		}
		return nil
	}
	if commandWrappers[strings.ToLower(res.headCanon)] {
		if inner := extractWrapperInner(res.rest); inner != "" {
			return CheckGitWrites(inner)
		}
	}
	return nil
}

// gitSubcommand finds git's subcommand token, skipping global options that
// take a value (-C <path>, -c <config>) and valueless flags.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "-C" || tok == "-c":
			i++
		case strings.HasPrefix(tok, "-"):
			// --git-dir=x and friends carry their value inside the token.
		default:
			return tok
		}
	}
	return ""
}

func gitWriteDecision(sub string, rest []string, segment string) error {
	if sub == "branch" {
		for _, tok := range rest {
			if gitBranchDeleteFlags[tok] {
				return &GitWriteError{Subcommand: "branch", Snippet: segment}
			}
		}
		return nil
	}
	if gitWriteSubcommands[sub] {
		return &GitWriteError{Subcommand: sub, Snippet: segment}
	}
	return nil
}

// IsLinkedWorktree reports whether dir (or one of its ancestors) is a git
// LINKED worktree: its repository anchor is a `.git` FILE pointing at
// `<main>/.git/worktrees/<name>`, as opposed to the main checkout's `.git`
// directory. This is the gating predicate for the git-write guard: runs
// rooted in a linked worktree are delegated runs whose contract says the
// orchestrator commits; main-checkout runs keep today's behavior.
func IsLinkedWorktree(dir string) bool {
	current := dir
	for {
		gitPath := filepath.Join(current, ".git")
		if info, err := os.Stat(gitPath); err == nil && !info.IsDir() {
			data, readErr := os.ReadFile(gitPath)
			// Git writes the gitdir with forward slashes even on Windows;
			// accept either separator so hand-made fixtures and real
			// worktrees both match.
			if readErr == nil && strings.Contains(
				strings.ReplaceAll(string(data), "\\", "/"), "/worktrees/",
			) {
				return true
			}
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}
