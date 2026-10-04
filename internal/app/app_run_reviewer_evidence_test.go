// Design step 4 tests (docs/plans/2026-10-02-reviewer-pass-verification.md
// §4, §8): T6 (the reviewer prompt carries the computed evidence), T7 (the
// final-text quality flags), T8 (the final answer's source), T9 (the recovered
// test/build commands) and T11 (the block's hard size cap).

package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T7: `finalTextFlags` is pure, so the whole table runs without a database, a
// provider or a repository. Every case asserts the exact slice, and each
// branch is reachable on its own — replacing any one of them with a bare
// `return nil` breaks its case, because the case then falls through to the
// next (weaker) diagnosis.
//
// The chain is exclusive on purpose: a 6-rune mojibake stub is ONE flag
// ("garbage_chars"), not "garbage_chars, very_short, possibly_truncated". The
// design lists the predicates; the table below pins the single diagnosis each
// input is expected to produce.
func TestFinalTextFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []string
	}{
		{"empty flag", "", []string{"empty"}},
		{"very short", "Report: done, reviewed, closed.", []string{"very_short"}},
		{"garbage chars", "ok\ufffdbad", []string{"garbage_chars"}},
		{
			name: "unexpected script",
			text: "Русский текст " + strings.Repeat("中文", 30),
			want: []string{"unexpected_script"},
		},
		{
			name: "repeated lines",
			text: strings.Repeat("a repeated line longer than twenty runes\n", 3),
			want: []string{"repeated_lines"},
		},
		{
			name: "repeated phrases",
			text: strings.Repeat("alpha beta gamma delta epsilon zeta ", 3),
			want: []string{"repeated_phrases"},
		},
		{
			name: "possibly truncated",
			text: "a fine report that just stops",
			want: []string{"possibly_truncated"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, finalTextFlags(tt.text),
				"the exact flag slice is what the reviewer prompt makes it address")
		})
	}

	// CONTROL: a healthy Russian report naming real files and milestones. Long
	// enough not to be very_short, Latin+Cyrillic only, no repeated line or
	// 6-word shingle, ends in a period. No flag at all.
	const control = "Отчёт по прогону. Внёс правки в internal/agent/agent_turn_step.go и закрыл M6/M7: " +
		"тесты зелёные, сборка проходит, ревьюер отметил пункты P2. Всё проверено по файлам."
	require.Nil(t, finalTextFlags(control), "a healthy report must raise no flag")

	// The control's mirror: the SAME text without its final period now raises
	// exactly the truncation flag and nothing else — the chain is exclusive,
	// so it cannot also be labelled unexpected_script or repeated_*.
	require.Equal(t, []string{"possibly_truncated"}, finalTextFlags(strings.TrimSuffix(control, ".")))

	// Revert-checks, one per branch: each case below stops at the branch under
	// test, so a branch that stopped returning its label would hand back the
	// NEXT (weaker) diagnosis instead.
	revert := []struct {
		name string
		text string
		want string
	}{
		{"empty branch", "", "empty"},
		{"garbage branch", "ok\ufffdbad", "garbage_chars"},
		{"script branch", "Русский текст " + strings.Repeat("中文", 30), "unexpected_script"},
		{"lines branch", strings.Repeat("a repeated line longer than twenty runes\n", 3), "repeated_lines"},
		{"phrases branch", strings.Repeat("alpha beta gamma delta epsilon zeta ", 3), "repeated_phrases"},
		{"truncation branch", "a fine report that just stops", "possibly_truncated"},
		{"very short branch", "too short", "very_short"},
	}
	for _, rc := range revert {
		t.Run("revert "+rc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, []string{rc.want}, finalTextFlags(rc.text),
				"replacing this branch with `return nil` would fall through to a weaker flag")
		})
	}
}

// T8: `finalTextSource` reads which user row the final assistant turn was
// answering. The fixture mirrors the observed r3 degeneracy: the orchestrator's
// real report, then a background-job notice, then a one-line reaction that
// became the session's final_text.
func TestFinalTextSource(t *testing.T) {
	t.Parallel()

	endTurn := message.Finish{Reason: message.FinishReasonEndTurn}
	assistantRow := func(id, text string) message.Message {
		return message.Message{
			ID: id, Role: message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: text}, endTurn},
		}
	}
	userRow := func(id, text string, notice bool) message.Message {
		return message.Message{
			ID: id, Role: message.User, BackgroundJobNotice: notice,
			Parts: []message.ContentPart{message.TextContent{Text: text}},
		}
	}

	t.Run("reaction to background notice", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			userRow("u1", "do the thing", false),
			assistantRow("a1", "the real report of the work"),
			// r3's 6bcf3ca2: the notice that buried the real report.
			userRow("u2", "Background job j1 (`go build`) finished: exit 0, ran 1s.", true),
			// r3's a7f0dbc6: the short reaction that became final_text.
			assistantRow("a2", "finalmente"),
		}
		require.Equal(t, "reaction_to_background_notice", finalTextSource(msgs))
	})

	t.Run("answer turn", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			userRow("u1", "do the thing", false),
			assistantRow("a1", "the report of the work"),
			// A later PLAIN user row (an ordinary follow-up) is not a notice.
			userRow("u2", "and also check the tests", false),
			assistantRow("a2", "checked them, all green."),
		}
		require.Equal(t, "answer_turn", finalTextSource(msgs))
	})

	t.Run("notice after a completed assistant turn still counts", func(t *testing.T) {
		t.Parallel()
		// The r3 shape with an extra completed turn after the short reaction:
		// the notice is still the row the final answer was answering.
		msgs := []message.Message{
			userRow("u1", "do the thing", false),
			assistantRow("a1", "the real report of the work"),
			userRow("u2", "Background job j2 (`go test ./...`) finished: exit 1, ran 9s.", true),
			assistantRow("a2", "finalmente"),
			assistantRow("a3", "finalmente"),
		}
		require.Equal(t, "reaction_to_background_notice", finalTextSource(msgs))
	})

	t.Run("the reviewer prompt row is never the source", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			userRow("u1", "do the thing", false),
			assistantRow("a1", "the report of the work"),
			userRow("u2", reviewerPassPrompt, false),
		}
		require.Equal(t, "answer_turn", finalTextSource(msgs))
	})
}

// T9: `commandResults` recovers what actually ran and its exit code. The
// messages are in-memory literals (cheaper than the DB-embedded fixtures the
// loop tests use) and carry CreatedAt values above `since`, so the run window
// is exercised too.
func TestCommandResultsFromTranscript(t *testing.T) {
	t.Parallel()

	const since = int64(1000)
	bashCall := func(id, cmd string, at int64) message.Message {
		return message.Message{
			ID: id, Role: message.Assistant, CreatedAt: at,
			Parts: []message.ContentPart{message.ToolCall{ID: id, Name: "bash", Input: `{"command":"` + cmd + `"}`}},
		}
	}
	toolRow := func(id string, tr message.ToolResult) message.Message {
		return message.Message{
			ID: id + "-res", Role: message.Tool, CreatedAt: 1001,
			Parts: []message.ContentPart{tr},
		}
	}

	t.Run("sync failure is reported with its exit code", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			bashCall("c1", "go test ./x", 1002),
			toolRow("c1", message.ToolResult{
				ToolCallID: "c1", Name: "bash", IsError: true,
				Content: "--- FAIL: TestX (0.00s)\nExit code 1",
			}),
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 1)
		require.Equal(t, "go test ./x", got[0].Cmd)
		require.Equal(t, 1, got[0].Exit)
		require.True(t, got[0].Known, "a sync result's code IS known (zero when no failure tail)")
		require.False(t, got[0].Async)
	})

	t.Run("sync success is exit zero", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			bashCall("c1", "go build ./...", 1002),
			toolRow("c1", message.ToolResult{ToolCallID: "c1", Content: "ok"}),
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 1)
		require.Equal(t, 0, got[0].Exit)
		require.True(t, got[0].Known)
	})

	t.Run("async job takes its exit from the notice", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			bashCall("c1", "go build ./...", 1002),
			toolRow("c1", message.ToolResult{
				ToolCallID: "c1", Name: "bash", Content: "Async bash job j1 started.",
				Metadata: `{"async":true,"job_id":"j1","status":"running"}`,
			}),
			{
				ID: "n1", Role: message.User, CreatedAt: 1020, BackgroundJobNotice: true,
				Parts: []message.ContentPart{message.TextContent{Text: "Background job j1 (`go build`) finished: exit 0, ran 1s."}},
			},
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 1)
		require.True(t, got[0].Async)
		require.Equal(t, 0, got[0].Exit)
		require.True(t, got[0].Known)
	})

	t.Run("async job with a nonzero notice", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			bashCall("c1", "go test ./internal/app", 1002),
			toolRow("c1", message.ToolResult{
				ToolCallID: "c1", Content: "Async bash job j2 started.",
				Metadata: `{"async":true,"job_id":"j2","status":"running"}`,
			}),
			{
				ID: "n2", Role: message.User, CreatedAt: 1020, BackgroundJobNotice: true,
				Parts: []message.ContentPart{message.TextContent{Text: "Background job j2 (`go test ./internal/app`) finished: exit 2, ran 4s."}},
			},
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 1)
		require.Equal(t, 2, got[0].Exit)
		require.True(t, got[0].Known)
	})

	t.Run("async job with no notice is unknown", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			bashCall("c1", "go test ./...", 1002),
			toolRow("c1", message.ToolResult{
				ToolCallID: "c1", Content: "Async bash job j3 started.",
				Metadata: `{"async":true,"job_id":"j3","status":"running"}`,
			}),
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 1)
		require.True(t, got[0].Async)
		require.False(t, got[0].Known, "no notice: the job may still be running")
	})

	t.Run("a timed-out async job is unknown, not exit zero", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			bashCall("c1", "go test ./...", 1002),
			toolRow("c1", message.ToolResult{
				ToolCallID: "c1", Content: "Async bash job j4 started.",
				Metadata: `{"async":true,"job_id":"j4","status":"running"}`,
			}),
			{
				ID: "n4", Role: message.User, CreatedAt: 1020, BackgroundJobNotice: true,
				Parts: []message.ContentPart{message.TextContent{Text: "Async job j4 (bash) timed out after 600s and was stopped. Partial output:\n\nx"}},
			},
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 1)
		require.True(t, got[0].Async)
		require.False(t, got[0].Known, "a timed-out job has no exit code to report")
	})

	t.Run("unrelated commands are excluded, run_command is included", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			bashCall("c1", "ls -la", 1002),
			{
				ID: "c2", Role: message.Assistant, CreatedAt: 1003,
				Parts: []message.ContentPart{message.ToolCall{
					ID: "c2", Name: "run_command", Input: `{"program":"go","args":["build","./..."]}`,
				}},
			},
			bashCall("c3", "go vet ./internal/app", 1004),
			{
				ID: "c4", Role: message.Assistant, CreatedAt: 1005,
				Parts: []message.ContentPart{message.ToolCall{ID: "c4", Name: "bash", Input: `{"command":"cat foo.txt"}`}},
			},
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 2, "only the two test/build commands survive the filter")
		require.Equal(t, "go build ./...", got[0].Cmd, "run_command renders program + args")
		require.Equal(t, "go vet ./internal/app", got[1].Cmd)
	})

	t.Run("messages before the run window are ignored", func(t *testing.T) {
		t.Parallel()
		msgs := []message.Message{
			// A previous run on the same session: 999 < since.
			bashCall("old", "go test ./old", 999),
			{
				ID: "old-res", Role: message.Tool, CreatedAt: 999,
				Parts: []message.ContentPart{message.ToolResult{ToolCallID: "old", Content: "Exit code 1"}},
			},
			bashCall("new", "go test ./new", 1002),
			{
				ID: "new-res", Role: message.Tool, CreatedAt: 1003,
				Parts: []message.ContentPart{message.ToolResult{ToolCallID: "new", Content: "Exit code 1"}},
			},
		}
		got := commandResults(msgs, since)
		require.Len(t, got, 1)
		require.Equal(t, "go test ./new", got[0].Cmd)
	})

	t.Run("only the last eight are reported", func(t *testing.T) {
		t.Parallel()
		var msgs []message.Message
		for i := range 11 {
			id := fmt.Sprintf("c%02d", i)
			msgs = append(msgs, bashCall(id, fmt.Sprintf("go test ./pkg%02d", i), 2000+int64(i)))
		}
		got := commandResults(msgs, since)
		require.Len(t, got, reviewEvidenceCommandCount)
		require.Equal(t, "go test ./pkg10", got[len(got)-1].Cmd, "the NEWEST command is the last row")
	})
}

// T11: `buildReviewEvidence` is pure, so the size cap is tested by handing it
// an oversized prompt, changed files, transcript and todos — all built as
// in-memory structs, no repository and no database.
//
// The inputs below are deliberately far bigger than the block can ever be, and
// they must stay that way: every per-section cap is in RUNES
// (reviewEvidenceRequestRunes, reviewEvidenceChangedLines, ...) while the
// overall cap reviewEvidenceMaxChars is compared in BYTES. A smaller input can
// leave the whole block under 6000 bytes, in which case no truncation happens
// at all and the assertions below would be vacuous. Only an input this large
// exercises the byte-level truncation path.
func TestReviewEvidence_SizeCapped(t *testing.T) {
	t.Parallel()

	before := gitSnapshot{
		OK: true, Head: "aaaaaaa", Branch: "feature/x",
		Status:  map[string]string{},
		Numstat: map[string]string{},
	}
	after := gitSnapshot{
		OK: true, Head: "bbbbbbb", Branch: "feature/x",
		Status:  map[string]string{},
		Numstat: map[string]string{},
	}
	// 200 files: changed_during_run prints at most
	// reviewEvidenceChangedLines rows and dirty_before_run at most
	// reviewEvidenceDirtyLines, so both sections hit their caps here.
	//
	// The status pair is IDENTICAL in both snapshots, so a path only counts as
	// changed because its numstat moved — that is what
	// `before.Numstat[path]="0\t1"` vs `after.Numstat[path]="0\t34"` encode.
	// Every OTHER dimension is capped: the prompt at reviewEvidenceRequestRunes,
	// the changed list at reviewEvidenceChangedLines rows, the dirty list at
	// reviewEvidenceDirtyLines rows, the commands at reviewEvidenceCommandCount
	// and the todos at reviewEvidenceTodoCount. The ONE input dimension that is
	// NOT capped anywhere is the LENGTH OF EACH PATH, because a printed row is
	// `  <status columns> <path> <numstat columns>` and the path itself is never
	// truncated. Long paths are therefore the only lever that can push the block
	// past reviewEvidenceMaxChars.
	// DO NOT "tidy" these paths back to short ones: with short paths the sections
	// only sum to ~4600 bytes, under the 6000-byte cap, so
	// `buildReviewEvidence` takes the "it fits" early return and never appends
	// the truncation marker — the assertions below would go vacuous and this
	// test would silently stop testing anything.
	for i := range 200 {
		// ~95 bytes per path: the 40 changed_during_run rows plus the 20
		// dirty_before_run rows alone are ~5700 bytes, which together with the
		// other sections carries the block comfortably past 6000 bytes.
		path := fmt.Sprintf("internal/module%02d/subpackage/nested/deeper/component_file_%03d_with_a_long_descriptive_name.go", i%20, i)
		before.Status[path] = " M"
		before.Numstat[path] = "0\t1"
		after.Status[path] = " M"
		after.Numstat[path] = "0\t34"
	}

	// 100 messages: the transcript is long enough on its own to overflow the
	// whole block regardless of the other sections.
	var msgs []message.Message
	for i := range 100 {
		msgs = append(msgs, message.Message{
			ID: fmt.Sprintf("a%02d", i), Role: message.Assistant, CreatedAt: 5000 + int64(i),
			Parts: []message.ContentPart{
				message.ToolCall{
					ID: fmt.Sprintf("t%02d", i), Name: "bash",
					Input: fmt.Sprintf(`{"command":"go test ./internal/pkg%02d"}`, i),
				},
				message.TextContent{Text: strings.Repeat("a long orchestrator claim about the work. ", 20)},
				message.Finish{Reason: message.FinishReasonEndTurn},
			},
		})
	}

	// 100 todos: only reviewEvidenceTodoCount are ever printed.
	var todos []session.Todo
	for i := range 100 {
		todos = append(todos, session.Todo{
			Content: fmt.Sprintf("an open todo item number %d with enough text to matter", i),
			Status:  session.TodoStatusPending,
		})
	}

	basis := &reviewBasis{
		// Far more than reviewEvidenceRequestRunes runes, so the request
		// section is capped at its rune limit rather than printing the prompt
		// in full.
		Prompt:     strings.Repeat("the original request text goes on and on. ", 400),
		Start:      time.Unix(5000, 0),
		WorkingDir: t.TempDir(),
		Before:     before,
	}

	out := buildReviewEvidence(basis, after, msgs, todos)

	require.LessOrEqual(t, len(out), reviewEvidenceMaxChars,
		"the whole block is hard-capped at reviewEvidenceMaxChars")
	require.True(t, strings.HasSuffix(out, "(evidence truncated)\n</review_evidence>"),
		"a capped block ends with the truncation marker, not mid-sentence")
	require.Contains(t, out, `<review_evidence trust="computed by rush, not by the orchestrator">`)
	// The cap is a byte cut, so the sections behind it are gone by construction:
	// only the two the block STARTED with are guaranteed to survive. Their
	// presence proves the truncation really cut the listing rather than
	// replacing it.
	require.Contains(t, out, "changed_during_run:")
	require.Contains(t, out, "dirty_before_run:")

	// A block whose sections ALL fit carries every heading and no marker at
	// all — that is the other half of the cap's contract, and it needs its own
	// (smaller) input: hand the same empty snapshots to a short prompt.
	fits := buildReviewEvidence(&reviewBasis{
		Start: time.Unix(5000, 0), Prompt: "implement the reviewer pass per the design doc",
		WorkingDir: t.TempDir(), Before: gitSnapshot{OK: true, Status: map[string]string{}},
	}, gitSnapshot{OK: true, Status: map[string]string{}}, msgs, todos)
	for _, section := range []string{
		"changed_during_run:", "dirty_before_run:", "open_todos:",
		"tool_calls_this_run:", "final_text_checks:", "test_or_build_commands:",
	} {
		require.Contains(t, fits, section)
	}
	require.Contains(t, fits, "note: worker sub-sessions are not scanned; check worker claims in files or with read_delegation_transcript")
	require.NotContains(t, fits, "(evidence truncated)")

	// A block that fits needs no marker at all.
	small := buildReviewEvidence(&reviewBasis{
		Start: time.Unix(5000, 0), Prompt: "small request", WorkingDir: t.TempDir(),
		Before: gitSnapshot{OK: false},
	}, gitSnapshot{OK: false}, nil, nil)
	require.NotContains(t, small, "(evidence truncated)")
	require.Contains(t, small, "git: not a repository")
	require.Contains(t, small, "no_test_or_build_command_seen: true")
}

// T6: the reviewer turn's prompt carries the computed evidence, built from two
// real git snapshots taken around a real edit. Skipped when git is missing.
func TestReviewerPass_PromptCarriesEvidence(t *testing.T) {
	if _, err := platform.Command(context.Background(), "git", "--version").Output(); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
		return string(out)
	}
	git("init", "--initial-branch=main")
	git("config", "user.email", "probe@example.com")
	git("config", "user.name", "probe")

	// A file that is ALREADY dirty before the run starts (the shared-tree
	// guard: web/dist/.gitkeep in the design's own example). It is written and
	// then left alone, never `git add`ed, so it is dirty rather than staged:
	// `--porcelain=v1` defaults to the `normal` untracked mode, which lists a
	// root-level untracked FILE on its own `??` line (only a whole untracked
	// directory is collapsed to a single `dir/` entry).
	require.NoError(t, os.WriteFile(filepath.Join(dir, "web-dist-.gitkeep"), []byte("x\n"), 0o644))

	// The file the run itself is about to change. It is committed clean, so the
	// run's edit is the only thing that moves it, and it must be tracked:
	// under the `normal` untracked mode nothing inside an untracked directory
	// is visible to either snapshot, because the whole untracked `internal/`
	// collapses to one `?? internal/` entry.
	target := filepath.Join(dir, "internal", "agent_turn_step.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte("package agent\n"), 0o644))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "initial.txt"), []byte("one\n"), 0o644))
	git("add", "initial.txt", target)
	git("commit", "-m", "initial")

	// The BEFORE snapshot: the .gitkeep is already dirty, the target is clean
	// (tracked and unmodified, so it is absent from status entirely).
	basis := &reviewBasis{
		Start:      time.Now().Add(-time.Minute),
		Prompt:     "implement the reviewer pass verification per the design doc",
		WorkingDir: dir,
		Before:     readGitSnapshot(context.Background(), dir),
	}
	require.True(t, basis.Before.OK, "the temp dir is a real repository")
	require.Contains(t, basis.Before.Status, "web-dist-.gitkeep",
		"the pre-existing dirty file must be in the before snapshot")
	require.NotContains(t, basis.Before.Status, "internal/agent_turn_step.go",
		"the file the run changes must not be in the before snapshot")

	// The run's edit.
	require.NoError(t, os.WriteFile(target, []byte("package agent\n\n// changed during the run\n"), 0o644))

	after := readGitSnapshot(context.Background(), dir)
	require.True(t, after.OK)
	require.Contains(t, after.Status, "internal/agent_turn_step.go",
		"the edit must show up in the after snapshot")

	out := buildReviewEvidence(basis, after, nil, nil)

	assert.Contains(t, out, `<review_evidence trust="computed by rush, not by the orchestrator">`)
	assert.Contains(t, out, "</review_evidence>")
	// The original prompt is what the reviewer judges the work against.
	assert.Contains(t, out, "implement the reviewer pass verification per the design doc")
	// The file the run touched is in changed_during_run...
	assert.Regexp(t, `(?s)changed_during_run:.*internal/agent_turn_step\.go`, out)
	// ...and NOT in dirty_before_run — the two lists are the whole point.
	dirty := sectionOf(t, out, "dirty_before_run:", "note:")
	assert.Contains(t, dirty, "web-dist-.gitkeep")
	assert.NotContains(t, dirty, "internal/agent_turn_step.go")
	// The always-appended caveat closes the block.
	assert.True(t, strings.HasSuffix(out,
		"note: worker sub-sessions are not scanned; check worker claims in files or with read_delegation_transcript\n</review_evidence>"))

	// Through reviewerTurnPrompt the marker arrives together with the block,
	// against a REAL session row and transcript (harness reuse, no duplicate).
	h := newReviewerPassApp(t, true)
	sess := createModelOverrideSession(t, h.app, "evidence-prompt")
	prompt := h.app.reviewerTurnPrompt(context.Background(), sess.ID, basis)
	assert.Contains(t, prompt, reviewerPassMarker, "the reviewer marker leads the prompt")
	assert.Contains(t, prompt, `<review_evidence trust="computed by rush, not by the orchestrator">`,
		"and the computed evidence block follows it")
	assert.Contains(t, prompt, "implement the reviewer pass verification per the design doc")

	// The evidence block as the reviewer will actually read it: the run's file
	// under changed_during_run, and only the pre-existing dirt under
	// dirty_before_run.
	promptChanges := sectionOf(t, prompt, "changed_during_run:", "dirty_before_run:")
	assert.Contains(t, promptChanges, "internal/agent_turn_step.go",
		"the run's edit is under changed_during_run in the prompt")
	promptDirty := sectionOf(t, prompt, "dirty_before_run:", "note:")
	assert.Contains(t, promptDirty, "web-dist-.gitkeep",
		"the pre-existing dirty file is under dirty_before_run in the prompt")
	assert.NotContains(t, promptDirty, "internal/agent_turn_step.go",
		"the run's own file is not reported as pre-existing dirt")

	// The nil-basis contract: a run with no reviewer configured gets the
	// prompt alone, byte-identical.
	assert.Equal(t, reviewerPassPrompt, h.app.reviewerTurnPrompt(context.Background(), sess.ID, nil))
}

// sectionOf returns the block between two section headers, trimmed.
func sectionOf(t *testing.T, block, from, to string) string {
	t.Helper()
	i := strings.Index(block, from)
	require.GreaterOrEqual(t, i, 0, "missing section %q", from)
	rest := block[i+len(from):]
	if j := strings.Index(rest, "\n"+to); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// ensure the config import stays used by the harness reuse above.
var _ = config.SelectedModelTypeSmart
