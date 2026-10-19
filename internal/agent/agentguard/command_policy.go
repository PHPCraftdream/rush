// Package agentguard provides command guards for Rush execution surfaces.
// The CI-only policy is accidental prevention, not a sandbox: interpreters,
// copied binaries, and scripts that spawn compilers can evade command-name checks.
package agentguard

import (
	"os"
	"strings"

	"github.com/PHPCraftdream/rush/internal/shell"
)

const denyExecEnv = "RUSH_DENY_EXEC"

var processCommandPolicy = NewCommandPolicy(os.Getenv(denyExecEnv))

type (
	CommandPolicy      struct{ rules map[string]struct{} }
	CommandDeniedError struct{ Rule string }
)

var ciOnlyRules = []string{"go", "linters", "js", "node", "test-runners", "python", "cargo"}

// NewCommandPolicy parses named rules, or expands ci-only to all built-ins.
func NewCommandPolicy(value string) CommandPolicy {
	rules := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		name := strings.ToLower(strings.TrimSpace(item))
		if name == "ci-only" {
			for _, rule := range ciOnlyRules {
				rules[rule] = struct{}{}
			}
		} else {
			switch name {
			case "go", "linters", "js", "node", "test-runners", "python", "cargo":
				rules[name] = struct{}{}
			}
		}
	}
	return CommandPolicy{rules: rules}
}

// CheckCommand and CheckArgs use shell's shared wrapper/argv candidate parser.
func (p CommandPolicy) CheckCommand(command string) error {
	calls, err := shell.ParseCommandCandidates(command)
	if err != nil {
		return nil
	}
	for _, call := range calls {
		if err := p.CheckArgs(call); err != nil {
			return err
		}
	}
	return nil
}

func (p CommandPolicy) CheckArgs(args []string) error {
	candidates := shell.CommandCandidatesForPlatform(args, true)
	candidates = append(candidates, augmentPolicyWrappers(args)...)
	return p.checkCandidates(candidates)
}

// CheckCommandPolicy evaluates a shell command using the process environment's policy.
func CheckCommandPolicy(command string) error {
	return processCommandPolicy.CheckCommand(command)
}

// CheckCommandPolicyArgs evaluates structured argv using the process environment's policy.
func CheckCommandPolicyArgs(args []string) error {
	return processCommandPolicy.CheckArgs(args)
}

func (e *CommandDeniedError) Error() string {
	return "the command is denied by the CI-only policy (RUSH_DENY_EXEC): tests/builds/lints run only in GitHub CI; matched rule " + e.Rule
}

func matchPolicy(args []string, enabled map[string]struct{}) string {
	if len(args) == 0 {
		return ""
	}
	head := policyCanonicalExecutable(args[0])
	has := func(r string) bool { _, ok := enabled[r]; return ok }
	sub := ""
	if len(args) > 1 {
		sub = strings.ToLower(args[1])
	}
	switch {
	case has("go") && head == "go":
		i := 1
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			t := args[i]
			if t == "-C" {
				i += 2
				continue
			}
			if strings.HasPrefix(t, "-C=") {
				i++
				continue
			}
			i++
		}
		if i < len(args) {
			s := strings.ToLower(args[i])
			if s == "mod" && i+1 < len(args) && strings.EqualFold(args[i+1], "tidy") {
				return "go mod tidy"
			}
			if strings.Contains("|vet|build|test|run|install|list|generate|tool|get|", "|"+s+"|") {
				return "go " + s
			}
		}
	case has("linters") && strings.Contains("|golangci-lint|staticcheck|revive|sqlc|gopls|gotestsum|ginkgo|", "|"+head+"|"):
		return head
	case has("js") && strings.Contains("|npm|pnpm|yarn|npx|bun|", "|"+head+"|") && (sub == "test" || sub == "build" || sub == "exec" || (sub == "run" && len(args) > 2 && (strings.EqualFold(args[2], "test") || strings.EqualFold(args[2], "build")))):
		return head + " " + sub
	case has("node") && head == "node":
		for i := 1; i < len(args); i++ {
			a := strings.ToLower(args[i])
			if a == "--test" {
				return "node --test"
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			if strings.Contains(a, "test") || strings.Contains(a, "build") {
				return "node test/build script"
			}
			break
		}
	case has("test-runners") && strings.Contains("|playwright|vitest|jest|pytest|", "|"+head+"|"):
		return head
	case has("python") && (head == "python" || head == "python3") && len(args) > 2 && args[1] == "-m" && (args[2] == "pytest" || args[2] == "unittest"):
		return "python -m " + args[2]
	case has("cargo") && head == "cargo" && strings.Contains("|build|test|check|clippy|", "|"+sub+"|"):
		return "cargo " + sub
	}
	return ""
}
func init() { shell.RegisterCommandPolicy(processCommandPolicy.CheckArgs) }
