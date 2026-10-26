package agentguard

import (
	"testing"
)

var preRound3AllowedCommands = []string{"gofmt -l .", "git status", "grep go test", "node script.js", "npm run lint", "node script.js test", "go fmt ./...", "env FOO=1 go fmt ./...", "xargs echo allowed", "find . -exec echo allowed \\;", "capm 2g idle go fmt ./...", "capm 2g go fmt ./..."}

var preRound3DeniedCommands = []string{
	"go vet ./...", "go build ./...", "go test ./...", "go run .", "go install ./...", "go list ./...", "go generate ./...", "go get example.com/x", "go tool vet ./...", "go mod tidy",
	"go -C dir test ./...", "go -C=dir test ./...", "go -x -n vet ./...", "Go.EXE -C dir test",
	"GOFLAGS=-count=1 go test ./...", "cmd /c go build", "powershell -Command \"go test ./...\"", "bash -c 'cd x && go vet ./...'", "capm 2g go test -p 1 ./...", "env FOO=1 go list ./...", "xargs go build", "npm run build", "npm test", "npx playwright test", "python -m pytest", "find . -exec go vet {} ;", "pint 1 go test", "capm 2g idle go test", "sudo -u builder go test ./...", "start \"title\" go test ./...",
	"golangci-lint run", "pytest", "cargo test", "node --test", "pnpm test",
}

var preRound3DeniedArgvs = [][]string{
	{`C:\Program Files\Go\bin\go.exe`, "vet", "./..."},
	{"capm", "2g", "idle", "go", "test", "./..."},
	{"xargs", "-n", "1", "go", "test", "./..."},
	{"xargs", "--max-args=1", "go", "test", "./..."},
	{"find", ".", "-exec", "go", "vet", "{}", ";"},
	{"env", "FOO=1", "go", "build", "./..."},
	{"cmd", "/c", "go", "test", "./..."},
	{"sudo", "-u", "builder", "go", "test", "./..."},
	{"start", "go", "test", "./..."},
	{"start", "title", "go", "test", "./..."},
	{"npx", "playwright", "test"},
	{"sudo", "go", "test", "./..."},
	{"npm", "test"},
	{"node", "--test"},
	{"python3", "-m", "pytest"},
	{"python", "-m", "unittest"},
	{"pytest"},
	{"golangci-lint", "run"},
	{"staticcheck", "./..."},
	{"revive", "./..."},
	{"sqlc", "generate"},
	{"gopls", "check"},
	{"gotestsum", "./..."},
	{"ginkgo", "./..."},
	{"playwright", "test"},
	{"vitest", "run"},
	{"jest", "--runInBand"},
	{"cargo", "build"},
	{"cargo", "test"},
	{"cargo", "check"},
	{"cargo", "clippy"},
}

var preRound3AllowedArgvs = [][]string{
	{"node", "script.js", "test"},
	{"capm", "2g", "idle", "go", "fmt", "./..."},
	{"go", "fmt", "./..."},
	{"npm", "run", "lint"},
	{"xargs", "echo", "allowed"},
	{"gofumpt", "-w", "file.go"},
	{"git", "status"},
	{"curl", "--version"},
	{"gh", "--version"},
	{".githooks/check_file_size.sh", "--selftest"},
	{".githooks/check_db_release_pairing_selftest.sh"},
}

func TestCommandPolicy(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	for _, command := range preRound3AllowedCommands {
		if err := p.CheckCommand(command); err != nil {
			t.Errorf("CheckCommand(%q) = %v", command, err)
		}
	}
	for _, command := range preRound3DeniedCommands {
		err := p.CheckCommand(command)
		if err == nil {
			t.Errorf("CheckCommand(%q) allowed", command)
		}
	}
}

func TestCommandPolicyArgvPathsAndWrappers(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	for _, argv := range preRound3DeniedArgvs {
		if err := p.CheckArgs(argv); err == nil {
			t.Errorf("CheckArgs(%q) allowed", argv)
		}
	}
	for _, argv := range preRound3AllowedArgvs {
		if err := p.CheckArgs(argv); err != nil {
			t.Errorf("CheckArgs(%q) = %v", argv, err)
		}
	}
}

func TestPreRound3CommandPolicyVerdictsUnchanged(t *testing.T) {
	// This net asserts the denial set is a SUPERSET of pre-round-3: every
	// previously denied argv is still denied, and every previously allowed
	// control is still allowed.
	p := NewCommandPolicy("ci-only")
	for _, command := range preRound3AllowedCommands {
		if err := p.CheckCommand(command); err != nil {
			t.Errorf("CheckCommand(%q) = %v", command, err)
		}
	}
	for _, command := range preRound3DeniedCommands {
		if err := p.CheckCommand(command); err == nil {
			t.Errorf("CheckCommand(%q) allowed", command)
		}
	}
	for _, argv := range preRound3DeniedArgvs {
		if err := p.CheckArgs(argv); err == nil {
			t.Errorf("CheckArgs(%q) allowed", argv)
		}
	}
	for _, argv := range preRound3AllowedArgvs {
		if err := p.CheckArgs(argv); err != nil {
			t.Errorf("CheckArgs(%q) = %v", argv, err)
		}
	}
}

func TestCommandPolicyNamedAndError(t *testing.T) {
	p := NewCommandPolicy("go")
	err := p.CheckArgs([]string{"go", "test", "./..."})
	want := "the command is denied by the CI-only policy (RUSH_DENY_EXEC): tests/builds/lints run only in GitHub CI; matched rule go test"
	if err == nil || err.Error() != want {
		t.Fatalf("CheckArgs denial: got %v, want %q", err, want)
	}
	if err := NewCommandPolicy("").CheckArgs([]string{"go", "test"}); err != nil {
		t.Fatalf("disabled policy: %v", err)
	}
	if err := NewCommandPolicy("js").CheckArgs([]string{"npm", "run", "lint"}); err != nil {
		t.Fatalf("named js rule overblocked lint: %v", err)
	}
	if err := NewCommandPolicy("node").CheckArgs([]string{"node", "script.js", "test"}); err != nil {
		t.Fatalf("named node rule overblocked ordinary script: %v", err)
	}
}
