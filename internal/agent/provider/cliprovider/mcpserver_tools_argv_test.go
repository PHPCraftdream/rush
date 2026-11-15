package cliprovider

import (
	"reflect"
	"testing"
)

func TestMCPGrepArgsSeparatesPatternAndPathFromOptions(t *testing.T) {
	for _, pattern := range []string{"--pre=./x.sh", "-e", "--files", "-n", "needle"} {
		t.Run(pattern, func(t *testing.T) {
			got := mcpGrepArgs(mcpGrepInput{Pattern: pattern, Glob: "*.go"}, "-n")
			want := []string{"grep", "-rn", "--color=never", "--max-count=100", "-e", pattern, "--include=*.go", "--", "-n"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("argv = %#v, want %#v", got, want)
			}
		})
	}
}
