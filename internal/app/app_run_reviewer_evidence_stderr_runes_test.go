// Regression tests for two defects of the review-evidence block:
//   - C9-15b: git stderr reached the porcelain parser on a successful
//     command, so a git warning became a branch name or a status entry;
//   - C9-19: the whole-block cap was compared in BYTES while the
//     per-section caps are in runes, halving the real cap for Cyrillic.
package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// cyrPath builds one Cyrillic path of exactly wantRunes runes (paths are the
// one section input that is never truncated, so they drive the block total).
func cyrPath(i, wantRunes int) string {
	base := fmt.Sprintf("путь/модуль/файл_обработчик_%02d.go", i)
	if pad := wantRunes - utf8.RuneCountInString(base); pad > 0 {
		base += strings.Repeat("ы", pad)
	}
	return base
}

// cyrillicSnapshots builds two snapshots whose n rows print Cyrillic paths of
// pathRunes runes each: identical status columns but moved numstat, so every
// path counts both as changed during the run and as dirty before it.
func cyrillicSnapshots(n, pathRunes int) (before, after gitSnapshot) {
	before = gitSnapshot{OK: true, Head: "aaaaaaa", Branch: "main", Status: map[string]string{}, Numstat: map[string]string{}}
	after = gitSnapshot{OK: true, Head: "bbbbbbb", Branch: "main", Status: map[string]string{}, Numstat: map[string]string{}}
	for i := range n {
		path := cyrPath(i, pathRunes)
		before.Status[path] = " M"
		before.Numstat[path] = "0\t1"
		after.Status[path] = " M"
		after.Numstat[path] = "0\t34"
	}
	return before, after
}

// cyrillicBasis carries a Cyrillic request above the 1500-rune request cap,
// so the request section prints exactly reviewEvidenceRequestRunes runes.
func cyrillicBasis(before gitSnapshot) *reviewBasis {
	return &reviewBasis{
		Start:      time.Unix(5000, 0),
		Prompt:     strings.Repeat("Проверить отчёт и исправления. ", 50),
		WorkingDir: "unused: buildReviewEvidence is pure",
		Before:     before,
	}
}

// C9-19, fitting half: a block of ~4600 runes but ~8800 bytes must NOT be
// truncated — the cap is reviewEvidenceMaxChars RUNES, matching the
// per-section caps. The old byte comparison cut it and dropped the note.
func TestReviewEvidence_CyrillicBlockFitsUnderRuneCap(t *testing.T) {
	before, after := cyrillicSnapshots(20, 60)

	out := buildReviewEvidence(cyrillicBasis(before), after, nil, nil)

	require.Greater(t, len(out), reviewEvidenceMaxChars,
		"the fixture must exceed the cap in BYTES for this test to say anything")
	require.LessOrEqual(t, utf8.RuneCountInString(out), reviewEvidenceMaxChars,
		"the fixture must stay within the cap in RUNES for this test to say anything")
	require.NotContains(t, out, "(evidence truncated)")
	require.True(t, strings.HasSuffix(out,
		"note: worker sub-sessions are not scanned; check worker claims in files or with read_delegation_transcript\n</review_evidence>"),
		"the closing note survives when the block fits its rune cap")
}

// C9-19, cutting half: past reviewEvidenceMaxChars RUNES the block is cut at
// a rune boundary and closed with the marker, exactly reviewEvidenceMaxChars
// runes long.
func TestReviewEvidence_RuneCapTruncatesAtRuneBoundary(t *testing.T) {
	before, after := cyrillicSnapshots(40, 95)

	out := buildReviewEvidence(cyrillicBasis(before), after, nil, nil)

	require.Equal(t, reviewEvidenceMaxChars, utf8.RuneCountInString(out),
		"a capped block is exactly reviewEvidenceMaxChars runes")
	require.True(t, utf8.ValidString(out), "the cut must never split a UTF-8 sequence")
	require.True(t, strings.HasSuffix(out, "(evidence truncated)\n</review_evidence>"),
		"a capped block ends with the truncation marker")
}

// C9-15b: git stderr on a SUCCESSFUL command must not reach the porcelain
// parser. The fake git writes a warning to stderr, then valid porcelain v1
// --branch output to stdout, then a second warning, exit 0 — any merged
// order of the two streams used to corrupt Branch or Status.
func TestReviewEvidence_GitStderrIsNotParsedAsPorcelain(t *testing.T) {
	dir := t.TempDir()
	shimDir := t.TempDir()

	var script string
	if runtime.GOOS == "windows" {
		script = strings.Join([]string{
			"@echo off",
			"echo warning: could not open directory 'x/': Permission denied 1>&2",
			"echo ## main...origin/main",
			"echo ?? dirty_file.txt",
			"echo warning: late stderr line 1>&2",
			"exit /b 0",
		}, "\r\n") + "\r\n"
		require.NoError(t, os.WriteFile(filepath.Join(shimDir, "git.cmd"), []byte(script), 0o755))
	} else {
		script = strings.Join([]string{
			"#!/bin/sh",
			`echo "warning: could not open directory 'x/': Permission denied" >&2`,
			`echo "## main...origin/main"`,
			`echo "?? dirty_file.txt"`,
			`echo "warning: late stderr line" >&2`,
			"exit 0",
		}, "\n") + "\n"
		shim := filepath.Join(shimDir, "git")
		require.NoError(t, os.WriteFile(shim, []byte(script), 0o755))
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	snap := readGitSnapshot(context.Background(), dir)

	require.True(t, snap.OK, "the fake git exits zero, so the snapshot must be OK")
	require.Equal(t, "main", snap.Branch, "the branch header, not a stderr warning")
	require.Equal(t, map[string]string{"dirty_file.txt": "??"}, snap.Status,
		"no stderr warning may become a status entry")
	require.Empty(t, snap.Numstat)
}
