package main

import (
	"os"
	"strings"
	"testing"
)

func TestProcessStopInstalledBeforeStartupIO(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	start := strings.Index(source, "func main() {")
	if start < 0 {
		t.Fatal("main entry point not found")
	}
	stop := strings.Index(source[start:], "cmd.InstallProcessStop()")
	guard := strings.Index(source[start:], "platform.GuardSourceTreeRun()")
	if stop < 0 || guard < 0 || stop > guard {
		t.Fatal("process-stop handler must be installed before startup disk work")
	}
}
