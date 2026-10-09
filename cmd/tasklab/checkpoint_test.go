package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/lab"
)

func cliTestSaveCheckpoint(t *testing.T, title string) lab.Checkpoint {
	t.Helper()
	step := cliTestStep("init", "init", `{"op":"init","expected_revision":0,"items":["`+title+`"]}`)
	report, err := lab.Run(context.Background(), cliTestScenario(step), nil)
	if err != nil {
		t.Fatal(err)
	}
	return report.Checkpoint
}

func cliTestSaveDirectory(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(entries))
	for _, entry := range entries {
		got[entry.Name()] = true
	}
	if len(got) != len(want) {
		t.Fatalf("directory contents = %v, want %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("missing retained file %q: %v", name, got)
		}
	}
}

func cliTestSavePreserved(t *testing.T, path string, original []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("destination changed: read error=%v", err)
	}
}

func TestCLISaveCheckpointExactByteBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	checkpoint := cliTestSaveCheckpoint(t, "Boundary")
	baseline, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.TreeKey += tasktree.TreeKey(strings.Repeat("x", lab.MaxInputBytes-len(baseline)-1))
	if err := saveCheckpoint(path, checkpoint); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != lab.MaxInputBytes || original[len(original)-1] != '\n' {
		t.Fatal("save did not include newline in exact boundary")
	}
	if got := cliTestCheckpoint(t, path); !reflect.DeepEqual(got, checkpoint) {
		t.Fatal("exact boundary unreadable or changed")
	}
	cliTestSaveDirectory(t, dir, "checkpoint.json")
	checkpoint.TreeKey += "x"
	if err := saveCheckpoint(path, checkpoint); err == nil {
		t.Fatal("plus-one encoded checkpoint saved")
	}
	cliTestSavePreserved(t, path, original)
	cliTestSaveDirectory(t, dir, "checkpoint.json")
}

func TestCLISaveCheckpointReplacesAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	old := cliTestSaveCheckpoint(t, "Old")
	if err := saveCheckpoint(path, old); err != nil {
		t.Fatal(err)
	}
	replacement := cliTestSaveCheckpoint(t, "Replacement")
	if err := saveCheckpoint(path, replacement); err != nil {
		t.Fatal(err)
	}
	if got := cliTestCheckpoint(t, path); !reflect.DeepEqual(got, replacement) {
		t.Fatal("replacement lost full checkpoint data")
	}
	cliTestSaveDirectory(t, dir, "checkpoint.json")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("checkpoint permissions = %o", info.Mode().Perm())
		}
	}
}

func TestCLISaveInvalidCheckpointPreservesDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	checkpoint := cliTestSaveCheckpoint(t, "Retained")
	if err := saveCheckpoint(path, checkpoint); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.SchemaVersion = 0
	if err := saveCheckpoint(path, checkpoint); err == nil {
		t.Fatal("invalid checkpoint accepted")
	}
	cliTestSavePreserved(t, path, original)
	cliTestSaveDirectory(t, dir, "checkpoint.json")
}

func TestCLISaveRenameFailurePreservesNonemptyDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkpoint.json")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "sentinel")
	original := []byte("directory must survive")
	if err := os.WriteFile(sentinel, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveCheckpoint(path, cliTestSaveCheckpoint(t, "New")); err == nil {
		t.Fatal("rename over nonempty directory succeeded")
	}
	cliTestSavePreserved(t, sentinel, original)
	cliTestSaveDirectory(t, path, "sentinel")
	cliTestSaveDirectory(t, dir, "checkpoint.json")
}

type cliTestCheckpointFile struct {
	*os.File
	stage   string
	failure error
	wrote   bool
	synced  bool
	closed  bool
}

func (f *cliTestCheckpointFile) Write(data []byte) (int, error) {
	f.wrote = true
	if f.stage == "write" {
		n, err := f.File.Write(data[:len(data)/2])
		return n, errors.Join(err, f.failure)
	}
	return f.File.Write(data)
}

func (f *cliTestCheckpointFile) Sync() error {
	f.synced = true
	err := f.File.Sync()
	if f.stage == "sync" {
		return errors.Join(err, f.failure)
	}
	return err
}

func (f *cliTestCheckpointFile) Close() error {
	f.closed = true
	err := f.File.Close()
	if f.stage == "close" {
		return errors.Join(err, f.failure)
	}
	return err
}

func TestCLISaveFilesystemFailuresPreserveDestination(t *testing.T) {
	checkpoint := cliTestSaveCheckpoint(t, "New")
	for _, stage := range []string{"write", "sync", "close", "rename"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "checkpoint.json")
			original := []byte("original bytes must remain untouched")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			unrelated := filepath.Join(dir, ".tasklab-checkpoint-unrelated")
			if err := os.WriteFile(unrelated, original, 0o600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected " + stage + " failure")
			var file *cliTestCheckpointFile
			renameCalled := false
			ops := checkpointFileOps{
				createTemp: func(parent, pattern string) (checkpointFile, error) {
					if parent != dir {
						t.Fatalf("temp directory = %q, want %q", parent, dir)
					}
					realFile, err := os.CreateTemp(parent, pattern)
					if err != nil {
						return nil, err
					}
					file = &cliTestCheckpointFile{File: realFile, stage: stage, failure: failure}
					return file, nil
				},
				rename: func(source, destination string) error {
					renameCalled = true
					if !file.wrote || !file.synced || !file.closed || source != file.Name() || destination != path {
						t.Fatal("rename before write/sync/close or incorrect rename paths")
					}
					cliTestSavePreserved(t, path, original)
					if got := cliTestCheckpoint(t, source); !reflect.DeepEqual(got, checkpoint) {
						t.Fatal("rename received incomplete checkpoint")
					}
					return failure
				},
			}
			err := saveCheckpointWithOps(path, checkpoint, ops)
			if !errors.Is(err, failure) {
				t.Fatalf("error = %v, want injected failure", err)
			}
			if file == nil || !file.wrote || !file.closed || file.synced != (stage != "write") || renameCalled != (stage == "rename") {
				t.Fatal("failure did not stop later persistence stages or close temp")
			}
			cliTestSavePreserved(t, path, original)
			cliTestSavePreserved(t, unrelated, original)
			cliTestSaveDirectory(t, dir, "checkpoint.json", ".tasklab-checkpoint-unrelated")
		})
	}
}
