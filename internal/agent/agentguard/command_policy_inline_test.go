package agentguard

import "testing"

func TestParseInlineOption(t *testing.T) {
	cases := []struct {
		tool     string
		argv     []string
		code     string
		consumed int
	}{
		{"node", []string{"-e"}, "", 0},
		{"node", []string{"-p"}, "", 0},
		{"node", []string{"-pe"}, "", 0},
		{"node", []string{"-ep"}, "", 0},
		{"python", []string{"-c"}, "", 0},
		{"node", []string{"-e", "x"}, "x", 1},
		{"node", []string{"--eval", "x"}, "x", 1},
		{"node", []string{"-p", "x"}, "x", 1},
		{"node", []string{"--print", "x"}, "x", 1},
		{"node", []string{"-pe", "x"}, "x", 1},
		{"node", []string{"-ep", "x"}, "x", 1},
		{"node", []string{"-eCODE"}, "CODE", 0},
		{"node", []string{"--eval=CODE"}, "CODE", 0},
		{"node", []string{"--print=CODE"}, "CODE", 0},
		{"node", []string{"-eCODE"}, "CODE", 0},
		{"node", []string{"-pCODE"}, "CODE", 0},
		{"node", []string{"-peCODE"}, "CODE", 0},
		{"node", []string{"-epCODE"}, "CODE", 0},
		{"python", []string{"-c", "CODE"}, "CODE", 1},
		{"python3", []string{"-c", "CODE"}, "CODE", 1},
		{"py", []string{"-c", "CODE"}, "CODE", 1},
		{"python", []string{"-cCODE"}, "CODE", 0},
		{"python3", []string{"-cCODE"}, "CODE", 0},
		{"py", []string{"-cCODE"}, "CODE", 0},
	}
	for _, tc := range cases {
		code, consumed, ok := parseInlineOption(tc.tool, tc.argv, 0)
		if !ok || code != tc.code || consumed != tc.consumed {
			t.Errorf("parseInlineOption(%s,%q)=(%q,%d,%v)", tc.tool, tc.argv, code, consumed, ok)
		}
	}
}

func TestInlineCommandExtraction(t *testing.T) {
	p := NewCommandPolicy("ci-only")
	denied := [][]string{{"node", "-e", "require('child_process').spawn('go',['test','./...'])"}, {"python", "-c", "import subprocess; subprocess.run(['go','test','./...'])"}, {"node", "--eval=require('child_process').execSync('go test ./...')"}, {"node", "-pe", "require('child_process').execSync('go test ./...')"}, {"node", "-ep", "require('child_process').exec('go test ./...')"}, {"python", "-c", "import subprocess; subprocess.run('go test ./...', shell=True)"}, {"python3", "-cimport subprocess; subprocess.run(['go','test','./...'])"}, {"node", "-e", `require('child_process').spawnSync('pytest', [])`}, {"node", "-e", `require('child_process').spawnSync('staticcheck')`}, {"node", "-e", `require('child_process').execFileSync('golangci-lint')`}}
	for _, argv := range denied {
		if err := p.CheckArgs(argv); err == nil {
			t.Errorf("CheckArgs(%q) allowed", argv)
		}
	}
	allowed := [][]string{{"node", "-e", "arbitrary code"}, {"python", "-c", "import subprocess; subprocess.run('git status', shell=True)"}, {"node", "-e", `require('child_process').spawn('git',['status'])`}}
	for _, argv := range allowed {
		if err := p.CheckArgs(argv); err != nil {
			t.Errorf("CheckArgs(%q)=%v", argv, err)
		}
	}
}

func TestCommandPolicyOptionOperandTableCoverage(t *testing.T) {
	want := map[string]map[string]bool{"npm": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "pnpm": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "yarn": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "bun": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "npx": optionSet("--prefix -C --cwd -w --workspace --registry --userconfig --loglevel"), "python": optionSet("-W -X -c -m"), "python3": optionSet("-W -X -c -m"), "py": optionSet("-W -X -c -m"), "cargo": optionSet("--manifest-path -Z --config -C --color --target-dir -j"), "go": optionSet("-C"), "node": optionSet("--require -r --import --loader --env-file -e -p")}
	p := NewCommandPolicy("ci-only")
	if len(commandPolicyOptionOperands) != len(want) {
		t.Fatalf("operand tool rows got %d want %d", len(commandPolicyOptionOperands), len(want))
	}
	for tool, options := range want {
		actual, ok := commandPolicyOptionOperands[tool]
		if !ok {
			t.Errorf("missing operand tool row %s", tool)
			continue
		}
		if len(actual) != len(options) {
			t.Errorf("%s operand inventory differs: got %d want %d", tool, len(actual), len(options))
		}
		for option := range options {
			if !actual[option] {
				t.Errorf("%s missing operand %s", tool, option)
			}
		}
		for option := range actual {
			if !options[option] {
				t.Errorf("%s unexpected operand %s", tool, option)
			}
			operand := "test"
			action := []string{"test"}
			if tool == "go" {
				operand = "build"
				action = []string{"fmt"}
			} else if tool == "cargo" {
				operand = "build"
				action = []string{"fmt"}
			} else if tool == "node" {
				operand = "test"
				action = []string{"script.js"}
			} else if tool == "npm" || tool == "pnpm" || tool == "yarn" || tool == "bun" || tool == "npx" {
				action = []string{"install"}
			} else {
				operand = "pytest"
				action = []string{"script.py"}
			}
			if (tool == "python" || tool == "python3" || tool == "py") && option == "-c" {
				operand = "print('pytest')"
				action = nil
			}
			if (tool == "python" || tool == "python3" || tool == "py") && option == "-m" {
				operand = "build"
				action = nil
			}
			if tool == "node" && (option == "-e" || option == "-p") {
				operand = "test"
				action = nil
			}
			argv := []string{tool, option, operand}
			argv = append(argv, action...)
			if err := p.CheckArgs(argv); err != nil {
				t.Errorf("operand control %q: %v", argv, err)
			}
			if option == "-m" {
				argv = []string{tool, option, "pytest"}
				if err := p.CheckArgs(argv); err == nil {
					t.Errorf("removed -m operand control %q was allowed", argv)
				}
			}
			if option == "-c" {
				argv = []string{tool, option, "import subprocess; subprocess.run('go test')"}
				if err := p.CheckArgs(argv); err == nil {
					t.Errorf("removed -c operand control %q was allowed", argv)
				}
			}
		}
	}
}
