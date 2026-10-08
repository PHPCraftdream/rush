package cahgen

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type pipelineStep struct {
	Uses     string `yaml:"uses"`
	Run      string `yaml:"run"`
	Continue bool   `yaml:"continue-on-error"`
	If       string `yaml:"if"`
}
type pipelineJob struct {
	Continue bool           `yaml:"continue-on-error"`
	Needs    any            `yaml:"needs"`
	Steps    []pipelineStep `yaml:"steps"`
}
type pipeline struct {
	On     map[string]any         `yaml:"on"`
	Jobs   map[string]pipelineJob `yaml:"jobs"`
	Before struct {
		Hooks []string `yaml:"hooks"`
	} `yaml:"before"`
}

func readPipeline(t *testing.T, name string) pipeline {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
	require.NoError(t, err)
	var p pipeline
	require.NoError(t, yaml.Unmarshal(b, &p))
	var raw any
	require.NoError(t, yaml.Unmarshal(b, &raw))
	assertNoSchedule(t, raw)
	return p
}

func assertNoSchedule(t *testing.T, value any) {
	t.Helper()
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			require.NotContains(t, []string{"schedule", "cron"}, strings.ToLower(key))
			assertNoSchedule(t, child)
		}
	case []any:
		for _, child := range v {
			assertNoSchedule(t, child)
		}
	}
}

// Revert-check: npm/GoReleaser refresh precedes compilation.
func TestPipelineRefreshOrder(t *testing.T) {
	p := readPipeline(t, ".github/workflows/publish-fork-npm.yml")
	refresh, builds, goSetup, nodeSetup := -1, 0, -1, -1
	for i, step := range p.Jobs["publish"].Steps {
		if strings.HasPrefix(step.Uses, "actions/setup-go@") {
			goSetup = i
		}
		if strings.HasPrefix(step.Uses, "actions/setup-node@") {
			nodeSetup = i
		}
		if strings.Contains(step.Run, "go run ./internal/tools/cahsync") {
			require.Equal(t, "go run ./internal/tools/cahsync -mode refresh -strict", strings.TrimSpace(step.Run))
			require.Greater(t, i, goSetup)
			require.Greater(t, i, nodeSetup)
			require.NotEqual(t, -1, goSetup)
			require.NotEqual(t, -1, nodeSetup)
			require.False(t, step.Continue)
			require.Empty(t, step.If)
			refresh = i
		}
		if strings.Contains(step.Run, "go build ") {
			builds++
			require.Greater(t, i, refresh)
			require.NotEqual(t, -1, refresh)
		}
	}
	require.Equal(t, 1, builds)
	g := readPipeline(t, ".goreleaser.yml")
	require.NotEmpty(t, g.Before.Hooks)
	require.Equal(t, "go run ./internal/tools/cahsync -mode refresh -strict", g.Before.Hooks[0])
	completion, man := false, false
	for i, hook := range g.Before.Hooks {
		if strings.Contains(hook, "go run . ") {
			require.Positive(t, i)
			completion = completion || strings.Contains(hook, "completion")
			man = man || strings.Contains(hook, " man")
		}
	}
	require.True(t, completion)
	require.True(t, man)
	require.Contains(t, g.Before.Hooks, "sh -c 'cd web && npm ci && npm run build'")
}

// Revert-check: drift checks remain isolated and nonblocking.
func TestPipelineDriftIsIsolated(t *testing.T) {
	p := readPipeline(t, ".github/workflows/build.yml")
	checks := 0
	for _, job := range p.Jobs {
		goSetup, nodeSetup := false, false
		for _, step := range job.Steps {
			goSetup = goSetup || strings.HasPrefix(step.Uses, "actions/setup-go@")
			nodeSetup = nodeSetup || strings.HasPrefix(step.Uses, "actions/setup-node@")
			if !strings.Contains(step.Run, "go run ./internal/tools/cahsync") {
				continue
			}
			checks++
			require.True(t, job.Continue)
			require.Nil(t, job.Needs)
			require.True(t, goSetup && nodeSetup)
			require.Contains(t, step.Run, "go run ./internal/tools/cahsync -check -strict || {")
			require.Contains(t, step.Run, "::warning::")
			require.Contains(t, step.Run, "exit 1")
			for _, other := range job.Steps {
				require.NotContains(t, other.Run, "go test ")
			}
		}
		require.Nil(t, job.Needs)
	}
	require.Equal(t, 1, checks)
}

// Revert-check: build.go refresh precedes compilation.
func TestBuildRefreshBeforeCompilation(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("..", "..", "build.go"), nil, 0)
	require.NoError(t, err)
	var refresh token.Pos
	builds := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if s, ok := call.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "RefreshBuild" {
			if x, ok := s.X.(*ast.Ident); ok && x.Name == "cahgen" {
				refresh = call.Pos()
			}
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "run" {
			var args []string
			for _, a := range call.Args {
				if l, ok := a.(*ast.BasicLit); ok && l.Kind == token.STRING {
					v, e := strconv.Unquote(l.Value)
					require.NoError(t, e)
					args = append(args, v)
				}
			}
			if len(args) >= 2 && args[0] == "go" && args[1] == "build" {
				builds++
				require.NotZero(t, refresh)
				require.Less(t, refresh, call.Pos())
			}
		}
		return true
	})
	require.Equal(t, 1, builds)
}

// Revert-check: launch failures obey CI policy and inherit environment.
func TestBuildLaunchPolicyAndEnvironment(t *testing.T) {
	for _, ci := range []string{"", "false", "true"} {
		t.Run(ci, func(t *testing.T) {
			var log bytes.Buffer
			err := refreshBuild(context.Background(), "root", &log, &log, func(string) string { return ci }, func(cmd *exec.Cmd) error {
				require.Nil(t, cmd.Env)
				require.Equal(t, "root", cmd.Dir)
				require.Equal(t, []string{"go", "run", "./internal/tools/cahsync", "-mode", "refresh"}, cmd.Args)
				return errors.New("compile/start failure")
			})
			if ci == "true" {
				require.ErrorContains(t, err, "compile/start failure")
			} else {
				require.NoError(t, err)
				require.Contains(t, log.String(), "warning:")
			}
		})
	}
}
