package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildRgSearchCmdSeparatesPatternAndPathFromOptions(t *testing.T) {
	for _, pattern := range []string{"--pre=./x.sh", "-e", "--files", "-n", "--", "needle"} {
		t.Run(pattern, func(t *testing.T) {
			cmd := buildRgSearchCmd(context.Background(), "rg", pattern, "-n", "*.go", 3)
			want := []string{"rg", "--json", "-H", "-n", "-0", "-e", pattern, "-C", "3", "--glob", "*.go", "--", "-n"}
			if !reflect.DeepEqual(cmd.Args, want) {
				t.Fatalf("argv = %#v, want %#v", cmd.Args, want)
			}
		})
	}
}

func TestAppendRgIgnoreFilesKeepsOptionsBeforePathSeparator(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, ".gitignore")
	b := filepath.Join(dir, ".rushignore")
	if err := os.WriteFile(a, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		pattern string
		include string
	}{
		{name: "pattern separator", pattern: "--", include: "*.go"},
		{name: "include separator", pattern: "needle", include: "--"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := buildRgSearchCmd(context.Background(), "rg", test.pattern, "-n", test.include, 2)
			appendRgIgnoreFiles(cmd, dir)
			want := []string{"rg", "--json", "-H", "-n", "-0", "-e", test.pattern, "-C", "2", "--glob", test.include, "--ignore-file", a, "--ignore-file", b, "--", "-n"}
			if !reflect.DeepEqual(cmd.Args, want) {
				t.Fatalf("argv = %#v, want %#v", cmd.Args, want)
			}
		})
	}
}
