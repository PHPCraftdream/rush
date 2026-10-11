package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

type mutationShortWriter struct{}

func (mutationShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestMutationCLIOutputShortWrite(t *testing.T) {
	t.Parallel()
	w := cliOutput{writer: mutationShortWriter{}}
	n, err := w.Write([]byte("help"))
	if n != 3 || !errors.Is(err, io.ErrShortWrite) || !errors.Is(w.err, io.ErrShortWrite) {
		t.Fatalf("short write: n=%d err=%v retained=%v", n, err, w.err)
	}
	var diagnostics bytes.Buffer
	if code := runCLI([]string{"repl", "--help"}, strings.NewReader(""), mutationShortWriter{}, &diagnostics); code != 1 {
		t.Fatalf("short help exit=%d", code)
	}
}

func TestMutationCLILimitsInclusive(t *testing.T) {
	t.Parallel()
	args := []string{"repl", "--tree-key", "board", "--max-nodes", "10000", "--max-depth", "128", "--max-title-bytes", "65536", "--max-reason-bytes", "65536", "--max-tombstones", "10000", "--max-receipts", "10000"}
	var output, diagnostics bytes.Buffer
	if code := runCLI(args, strings.NewReader(""), &output, &diagnostics); code != 0 {
		t.Fatalf("inclusive limits exit=%d diagnostics=%s", code, diagnostics.String())
	}
}
