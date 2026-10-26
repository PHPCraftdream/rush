package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMainHelpAndInvalidInput(t *testing.T) {
	// V-a: validation precedes opening the events destination.
	var out, errout bytes.Buffer
	if code := Main(context.Background(), []string{"--help"}, &out, &errout); code != 0 || !bytes.Contains(out.Bytes(), []byte("replay")) {
		t.Fatal("help", code)
	}
	dir := t.TempDir()
	scenario := filepath.Join(dir, "invalid.json")
	journal := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(scenario, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Main(context.Background(), []string{"run", scenario, "--events", journal}, &out, &errout); code != 2 {
		t.Fatal("invalid exit", code)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("invalid input opened events")
	}
}
