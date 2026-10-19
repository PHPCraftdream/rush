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
	CommandDeniedError struct {
		Rule   string
		Detail string
	}
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
	if reason, opaque := p.checkOpaqueWrapper(args); opaque {
		return &CommandDeniedError{Rule: "opaque-wrapper-command", Detail: "opaque command head or payload " + reason + "; " + opaqueWrapperDetail}
	}
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
	return "the command is denied by the CI-only policy (RUSH_DENY_EXEC): tests/builds/lints run only in GitHub CI; matched rule " + e.Rule + func() string {
		if e.Detail == "" {
			return ""
		}
		return ": " + e.Detail
	}()
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
		positional, unknown := scanToolArgs("go", args[1:])
		if !unknown && len(positional) > 0 {
			positional = positional[:1]
		}
		for _, token := range positional {
			s := strings.ToLower(token)
			if s == "mod" && strings.Contains(strings.ToLower(strings.Join(args, " ")), "mod tidy") {
				return "go mod tidy"
			}
			if strings.Contains("|vet|build|test|run|install|list|generate|tool|get|", "|"+s+"|") {
				return "go " + s
			}
		}
	case has("linters") && strings.Contains("|golangci-lint|staticcheck|revive|sqlc|gopls|gotestsum|ginkgo|", "|"+head+"|"):
		return head
	case has("js") && strings.Contains("|npm|pnpm|yarn|npx|bun|", "|"+head+"|"):
		positional, unknown := scanToolArgs(head, args[1:])
		if unknown {
			for _, token := range positional {
				sub = strings.ToLower(token)
				if strings.Contains("|test|build|exec|", "|"+sub+"|") {
					return head + " " + sub
				}
			}
			break
		}
		if len(positional) > 0 {
			sub = strings.ToLower(positional[0])
			if sub == "test" || sub == "build" || sub == "exec" || (sub == "run" && len(positional) > 1 && (strings.EqualFold(positional[1], "test") || strings.EqualFold(positional[1], "build"))) {
				return head + " " + sub
			}
		}
	case has("node") && head == "node":
		positional, unknown := scanToolArgs("node", args[1:])
		if unknown {
			for _, a := range positional {
				a = strings.ToLower(a)
				if a == "--test" || strings.HasPrefix(a, "--test-") {
					return "node --test"
				}
				if !strings.HasPrefix(a, "-") && strings.Contains(a, "test") {
					return "node test script"
				}
			}
			break
		}
		for _, a := range positional {
			a = strings.ToLower(a)
			if a == "--test" || strings.HasPrefix(a, "--test-") {
				return "node --test"
			}
			if strings.HasPrefix(a, "-") {
				continue
			}
			if strings.Contains(a, "test") {
				return "node test script"
			}
			break
		}
	case has("test-runners") && strings.Contains("|playwright|vitest|jest|pytest|", "|"+head+"|"):
		return head
	case has("python") && (head == "python" || head == "python3" || head == "py"):
		positional, _ := scanToolArgs(head, args[1:])
		for i := 0; i+1 < len(positional); i++ {
			if positional[i] == "-m" && (positional[i+1] == "pytest" || positional[i+1] == "unittest") {
				return "python -m " + positional[i+1]
			}
		}
	case has("cargo") && head == "cargo":
		positional, _ := scanToolArgs("cargo", args[1:])
		for _, token := range positional {
			s := strings.ToLower(token)
			if strings.Contains("|build|test|check|clippy|", "|"+s+"|") {
				return "cargo " + s
			}
		}
	}
	return ""
}
func init() { shell.RegisterCommandPolicy(processCommandPolicy.CheckArgs) }
