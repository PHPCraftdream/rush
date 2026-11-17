package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const executeHelperEnv = "RUSH_TEST_EXECUTE_HELPER"

func TestExecuteHelperProcess(t *testing.T) {
	if os.Getenv(executeHelperEnv) != "1" {
		base := t.TempDir()
		t.Setenv("RUSH_GLOBAL_DIR", filepath.Join(base, "global-dir"))
		t.Setenv("RUSH_GLOBAL_DATA", filepath.Join(base, "data"))
		t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(base, "config"))
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(executeHelperEnv+"_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"rush"}, args...)
	Execute()
	os.Exit(0)
}

func cleanExecuteHelperEnv(env []string) []string {
	keys := []string{"RUSH_GLOBAL_DIR", "RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME", "USERPROFILE", executeHelperEnv, executeHelperEnv + "_ARGS"}
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		remove := false
		for _, blocked := range keys {
			if strings.EqualFold(key, blocked) {
				remove = true
				break
			}
		}
		if !remove {
			clean = append(clean, entry)
		}
	}
	return clean
}
