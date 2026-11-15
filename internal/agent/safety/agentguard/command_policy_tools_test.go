package agentguard

import "testing"

func TestCommandPolicyToolOptionExamples(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	denied := [][]string{
		{"npm", "--prefix", ".", "test"},
		{"pnpm", "-C", "app", "run", "build"},
		{"yarn", "--cwd", "app", "test"},
		{"python", "-I", "-m", "unittest"},
		{"python", "-W", "error", "-m", "pytest"},
		{"python3", "-m", "pytest"},
		{"py", "-3", "-m", "unittest"},
		{"cargo", "+stable", "build"},
		{"cargo", "--manifest-path", "x/Cargo.toml", "test"},
		{"node", "--require", "./setup.cjs", "--test"},
		{"go", "-C", "dir", "test"},
		{"npm", "--new", "test"},
		{"go", "--new", "dir", "test"},
		{"cargo", "--new", "dir", "build"},
		{"python", "--new", "-m", "pytest"},
		{"node", "--new", "--test"},
		{"node", "--test", "-e", "harmless"},
		{"node", "-e", `require('child_process').execSync('go test ./...')`},
		{"node", "--require", "./setup.cjs", "-e", `require('child_process').execSync('go test ./...')`},
		{"node", "-p", `require('child_process').execSync("go test ./...")`},
		{"node", "--require", "./setup.cjs", "--test-reporter", "spec"},
	}
	for _, argv := range denied {
		if err := p.CheckArgs(argv); err == nil {
			t.Errorf("CheckArgs(%q) allowed", argv)
		}
	}
	allowed := [][]string{
		{"node", "build.js"},
		{"node", "-e", "arbitrary code"},
		{"node", "-p", "test"},
		{"node", "script.js", "--test"},
		{"python", "-c", "arbitrary code"},
		{"python", "-c", "code", "-m", "pytest"},
		{"python", "-m", "pip", "install", "thing"},
		{"node", "-p", `require('child_process').execSync("git status")`},
		{"npm", "--prefix", "test", "install"},
		{"node", "--require", "./setup.cjs", "build.js"},
	}
	for _, argv := range allowed {
		if err := p.CheckArgs(argv); err != nil {
			t.Errorf("CheckArgs(%q) = %v", argv, err)
		}
	}
}

func TestCommandPolicyOptionOperandTableVerdicts(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	cases := []struct {
		argv []string
		deny bool
	}{
		{[]string{"npm", "--prefix", "root", "test"}, true},
		{[]string{"npm", "-C", "root", "build"}, true},
		{[]string{"npm", "--cwd", "root", "exec"}, true},
		{[]string{"npm", "-w", "pkg", "test"}, true},
		{[]string{"npm", "--workspace", "pkg", "test"}, true},
		{[]string{"npm", "--registry", "url", "test"}, true},
		{[]string{"npm", "--userconfig", "file", "test"}, true},
		{[]string{"npm", "--loglevel", "warn", "test"}, true},
		{[]string{"pnpm", "--prefix", "root", "test"}, true},
		{[]string{"pnpm", "-C", "root", "test"}, true},
		{[]string{"pnpm", "--cwd", "root", "test"}, true},
		{[]string{"pnpm", "-w", "pkg", "test"}, true},
		{[]string{"pnpm", "--workspace", "pkg", "test"}, true},
		{[]string{"pnpm", "--registry", "url", "test"}, true},
		{[]string{"pnpm", "--userconfig", "file", "test"}, true},
		{[]string{"pnpm", "--loglevel", "warn", "test"}, true},
		{[]string{"yarn", "--prefix", "root", "test"}, true},
		{[]string{"yarn", "-C", "root", "test"}, true},
		{[]string{"yarn", "--cwd", "root", "test"}, true},
		{[]string{"yarn", "-w", "pkg", "test"}, true},
		{[]string{"yarn", "--workspace", "pkg", "test"}, true},
		{[]string{"yarn", "--registry", "url", "test"}, true},
		{[]string{"yarn", "--userconfig", "file", "test"}, true},
		{[]string{"yarn", "--loglevel", "warn", "test"}, true},
		{[]string{"bun", "--prefix", "root", "test"}, true},
		{[]string{"bun", "-C", "root", "test"}, true},
		{[]string{"bun", "--cwd", "root", "test"}, true},
		{[]string{"bun", "-w", "pkg", "test"}, true},
		{[]string{"bun", "--workspace", "pkg", "test"}, true},
		{[]string{"bun", "--registry", "url", "test"}, true},
		{[]string{"bun", "--userconfig", "file", "test"}, true},
		{[]string{"bun", "--loglevel", "warn", "test"}, true},
		{[]string{"npx", "--prefix", "root", "test"}, true},
		{[]string{"npx", "-C", "root", "test"}, true},
		{[]string{"npx", "--cwd", "root", "test"}, true},
		{[]string{"npx", "-w", "pkg", "test"}, true},
		{[]string{"npx", "--workspace", "pkg", "test"}, true},
		{[]string{"npx", "--registry", "url", "test"}, true},
		{[]string{"npx", "--userconfig", "file", "test"}, true},
		{[]string{"npx", "--loglevel", "warn", "test"}, true},
		{[]string{"python", "-W", "error", "-m", "pytest"}, true},
		{[]string{"python", "-X", "dev", "-m", "unittest"}, true},
		{[]string{"python", "-c", "x"}, false},
		{[]string{"python", "-m", "pip"}, false},
		{[]string{"python3", "-W", "error", "-m", "pytest"}, true},
		{[]string{"python3", "-X", "dev", "-m", "unittest"}, true},
		{[]string{"python3", "-c", "x"}, false},
		{[]string{"python3", "-m", "pip"}, false},
		{[]string{"py", "-3", "-m", "unittest"}, true},
		{[]string{"py", "-W", "error", "-m", "pytest"}, true},
		{[]string{"py", "-X", "dev", "-m", "unittest"}, true},
		{[]string{"py", "-c", "x"}, false},
		{[]string{"py", "-m", "pip"}, false},
		{[]string{"cargo", "--manifest-path", "x/Cargo.toml", "test"}, true},
		{[]string{"cargo", "-Z", "unstable", "build"}, true},
		{[]string{"cargo", "--config", "x", "test"}, true},
		{[]string{"cargo", "-C", "x", "check"}, true},
		{[]string{"cargo", "--color", "always", "clippy"}, true},
		{[]string{"cargo", "--target-dir", "x", "build"}, true},
		{[]string{"cargo", "-j", "2", "test"}, true},
		{[]string{"go", "-C", "dir", "test"}, true},
		{[]string{"node", "--require", "file", "--test"}, true},
		{[]string{"node", "-r", "file", "--test"}, true},
		{[]string{"node", "--import", "file", "--test"}, true},
		{[]string{"node", "--loader", "file", "--test"}, true},
		{[]string{"node", "--env-file", "file", "--test"}, true},
	}
	for _, tc := range cases {
		err := p.CheckArgs(tc.argv)
		if (err != nil) != tc.deny {
			t.Errorf("CheckArgs(%q) err=%v, deny=%v", tc.argv, err, tc.deny)
		}
	}
}

func TestCommandPolicyOptionFlagVerdicts(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	cases := []struct {
		argv []string
		deny bool
	}{
		{[]string{"python", "-I", "-m", "pytest"}, true},
		{[]string{"python", "-E", "-m", "unittest"}, true},
		{[]string{"python", "-s", "-m", "pytest"}, true},
		{[]string{"python", "-S", "-m", "pytest"}, true},
		{[]string{"python", "-B", "-m", "pytest"}, true},
		{[]string{"python", "-u", "-m", "pytest"}, true},
		{[]string{"python", "-O", "-m", "pytest"}, true},
		{[]string{"python", "-OO", "-m", "pytest"}, true},
		{[]string{"python", "-q", "-m", "pytest"}, true},
		{[]string{"python", "-v", "-m", "pytest"}, true},
		{[]string{"python", "-x", "-m", "pytest"}, true},
		{[]string{"python", "-b", "-m", "pytest"}, true},
		{[]string{"python", "-d", "-m", "pytest"}, true},
		{[]string{"python", "-R", "-m", "pytest"}, true},
		{[]string{"python", "-P", "-m", "pytest"}, true},
		{[]string{"python3", "-I", "-m", "pytest"}, true},
		{[]string{"python3", "-E", "-m", "pytest"}, true},
		{[]string{"python3", "-s", "-m", "pytest"}, true},
		{[]string{"python3", "-S", "-m", "pytest"}, true},
		{[]string{"python3", "-B", "-m", "pytest"}, true},
		{[]string{"python3", "-u", "-m", "pytest"}, true},
		{[]string{"python3", "-O", "-m", "pytest"}, true},
		{[]string{"python3", "-OO", "-m", "pytest"}, true},
		{[]string{"python3", "-q", "-m", "pytest"}, true},
		{[]string{"python3", "-v", "-m", "pytest"}, true},
		{[]string{"python3", "-x", "-m", "pytest"}, true},
		{[]string{"python3", "-b", "-m", "pytest"}, true},
		{[]string{"python3", "-d", "-m", "pytest"}, true},
		{[]string{"python3", "-R", "-m", "pytest"}, true},
		{[]string{"python3", "-P", "-m", "pytest"}, true},
		{[]string{"py", "-I", "-m", "pytest"}, true},
		{[]string{"py", "-E", "-m", "pytest"}, true},
		{[]string{"py", "-s", "-m", "pytest"}, true},
		{[]string{"py", "-S", "-m", "pytest"}, true},
		{[]string{"py", "-B", "-m", "pytest"}, true},
		{[]string{"py", "-u", "-m", "pytest"}, true},
		{[]string{"py", "-O", "-m", "pytest"}, true},
		{[]string{"py", "-OO", "-m", "pytest"}, true},
		{[]string{"py", "-q", "-m", "pytest"}, true},
		{[]string{"py", "-v", "-m", "pytest"}, true},
		{[]string{"py", "-x", "-m", "pytest"}, true},
		{[]string{"py", "-b", "-m", "pytest"}, true},
		{[]string{"py", "-d", "-m", "pytest"}, true},
		{[]string{"py", "-R", "-m", "pytest"}, true},
		{[]string{"py", "-P", "-m", "pytest"}, true},
		{[]string{"npm", "--workspaces", "test"}, true},
		{[]string{"pnpm", "--workspaces", "test"}, true},
		{[]string{"yarn", "--workspaces", "test"}, true},
		{[]string{"bun", "--workspaces", "test"}, true},
		{[]string{"npx", "--workspaces", "test"}, true},
		{[]string{"cargo", "+stable", "build"}, true},
		{[]string{"go", "-x", "test"}, true},
		{[]string{"go", "-n", "test"}, true},
		{[]string{"go", "-v", "test"}, true},
		{[]string{"node", "--test"}, true},
		{[]string{"node", "--test-reporter", "spec"}, true},
		{[]string{"node", "build.js"}, false},
	}
	for _, tc := range cases {
		err := p.CheckArgs(tc.argv)
		if (err != nil) != tc.deny {
			t.Errorf("CheckArgs(%q) err=%v, deny=%v", tc.argv, err, tc.deny)
		}
	}
}
