package server

// Web composer history pollution — the regression pin.
//
// The web composer's ArrowUp recall list is derived client-side from the
// session's message rows ($myPrompts in web/src/store.ts), which are served
// through toMessageWire (load_messages reply, message_created/message_updated
// broadcasts). Before this fix the derivation only excluded hidden/summary/
// non-user rows, so EVERY user-role row surfaced in recall — including:
//
//   - CLI-originated prompts (`rush run`, `rush sessions inject`), stamped
//     message.OriginCLI at their recording sites (internal/cmd/run.go,
//     internal/cmd/sessions_inject.go);
//   - async completion notices ("Async job ... finished"), stamped
//     BackgroundJobNotice by internal/agent/coordinator_background.go — note
//     they carry OriginWeb too, so origin alone is NOT sufficient to exclude
//     them;
//   - Phase 4 autonomous idle-resume notices (AutoResumed + notice flags).
//
// The fix exposes message.Origin on MessageWire and teaches the derivation to
// require web-origin, non-notice, non-auto-resumed user rows. These tests pin
// both halves: the recording sites stamp authorship faithfully, and the
// serving surface carries it so the client filter can do its job.
//
// webComposerHistoryWire below is a deliberate MIRROR of the TypeScript
// derivation (web/src/store.ts $myPrompts + isAsyncCompletionNotice from
// web/src/asyncJobCompletion.ts). The frontend has no runnable unit-test
// harness in this checkout (web/node_modules is absent), so the behavior
// contract is pinned here at the serving boundary; any change must update
// both sides.
import (
	"regexp"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// asyncNoticePattern mirrors asyncJobCompletion.ts's asyncNoticePattern —
// legacy notice rows that predate the BackgroundJobNotice flag.
var asyncNoticePattern = regexp.MustCompile(`^Async job (\S+) \([^)]*\) (finished|failed)\.\n\n([\s\S]*)$`)

// webComposerHistoryWire projects a session's messages to the composer recall
// list exactly as the browser derives it: visible user messages with text,
// newest first, that were typed in the web composer, excluding notices
// (HumanTyped=false, or legacy-pattern for pre-flag rows) and autonomous
// idle-resume turns. HumanTyped is served on the wire (toMessageWire /
// isHumanTyped in wire.go) -- this function mirrors $myPrompts in
// web/src/store.ts, which filters on that SAME field plus the SAME legacy
// fallback (asyncJobCompletion.ts's isAsyncCompletionNotice).
func webComposerHistoryWire(msgs []message.Message) []string {
	var out []string
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if !toMessageWire(m).HumanTyped {
			continue
		}
		if asyncNoticePattern.MatchString(m.FullText()) {
			continue // legacy un-flagged rows, predating the BackgroundJobNotice column
		}
		text := strings.TrimSpace(m.FullText())
		if text == "" {
			continue
		}
		out = append(out, text)
	}
	return out
}

// createUserMsg records one user message through the real message service,
// with the authorship stamp its production recording site uses.
func createUserMsg(t *testing.T, msgs message.Service, sessionID string, params message.CreateMessageParams) message.Message {
	t.Helper()
	msg, err := msgs.Create(t.Context(), sessionID, params)
	require.NoError(t, err)
	return msg
}

// userText builds CreateMessageParams for a human-typed web prompt.
func userText(text string) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:   message.User,
		Parts:  []message.ContentPart{message.TextContent{Text: text}},
		Origin: message.OriginWeb,
	}
}

// TestMessageWire_OriginReachesTheBrowser pins the serving half of the fix:
// the authorship stamp recorded on the row must survive toMessageWire, or the
// client filter has nothing to filter on.
func TestMessageWire_OriginReachesTheBrowser(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		origin message.Origin
		want   string
	}{
		{"web typed prompt", message.OriginWeb, "web"},
		{"cli run prompt", message.OriginCLI, "cli"},
		{"sdk prompt", message.OriginSDK, "sdk"},
		{"legacy unspecified", message.OriginUnspecified, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wire := toMessageWire(message.Message{
				ID:     "m1",
				Role:   message.User,
				Origin: tc.origin,
				Parts:  []message.ContentPart{message.TextContent{Text: "hello"}},
			})
			require.Equal(t, tc.want, wire.Origin)

			if tc.want == "" {
				// Unspecified must not arrive as a fabricated value the
				// client could misread as another channel.
				require.NotEqual(t, "cli", wire.Origin)
				require.NotEqual(t, "sdk", wire.Origin)
			}
		})
	}
}

// TestWebComposerHistory_ServesOnlyWebTypedPrompts is the recording-site
// test: a CLI-originated message, an async completion notice, and a
// web-typed message all land in the same session; the composer-history
// projection returns ONLY the web-typed one.
//
// The notice is stamped OriginWeb on purpose — that is exactly how
// internal/agent/coordinator_background.go's notifyAsyncCompletion records
// it (WithCallOrigin(ctx, message.OriginWeb) plus the notice flags), proving
// the origin check alone cannot carry the fix and the notice flags are
// load-bearing.
func TestWebComposerHistory_ServesOnlyWebTypedPrompts(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "composer-history-mixed")
	require.NoError(t, err)

	// Recording site 1: CLI `rush run` prompt — OriginCLI, as stamped by
	// internal/cmd/run.go's run request and internal/cmd/sessions_inject.go.
	cliParams := userText("rush run prompt from the CLI")
	cliParams.Origin = message.OriginCLI
	cli := createUserMsg(t, a.Messages, sess.ID, cliParams)
	require.Equal(t, message.OriginCLI, cli.Origin, "recording site must stamp the CLI origin")

	// Recording site 2: async completion notice — OriginWeb + notice flag,
	// as stamped by coordinator.notifyAsyncCompletion (whose Phase 4 path
	// also sets AutoResumed).
	noticeParams := userText("Async job call-1 (bash) finished.\n\ndone")
	noticeParams.BackgroundJobNotice = true
	notice := createUserMsg(t, a.Messages, sess.ID, noticeParams)
	require.Equal(t, message.OriginWeb, notice.Origin, "notices carry the web origin — flags must exclude them")
	require.True(t, notice.BackgroundJobNotice)

	// Recording site 3: a genuine web-composer prompt — OriginWeb, no flags,
	// as stamped by the web send path (handlers_agent.go WithCallOrigin).
	web := createUserMsg(t, a.Messages, sess.ID, userText("fix the login bug"))
	require.Equal(t, message.OriginWeb, web.Origin)

	// An assistant reply between human prompts must not disturb anything.
	reply, err := a.Messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "on it"}},
	})
	require.NoError(t, err)
	reply.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, reply))

	// Serve the session exactly as handleLoadMessages does.
	msgs, _, err := a.Messages.ListWithWatermark(ctx, sess.ID)
	require.NoError(t, err)
	rows := toMessagesWire(msgs)

	// The serving surface must distinguish the channels...
	origins := make(map[string]string, len(rows))
	for _, row := range rows {
		origins[row.ID] = row.Origin
	}
	require.Equal(t, "cli", origins[cli.ID], "served CLI row must carry its origin")
	require.Equal(t, "web", origins[notice.ID], "served notice row must carry its origin")
	require.Equal(t, "web", origins[web.ID])

	// ...so the composer-history projection selects ONLY the web-typed one.
	require.Equal(t, []string{"fix the login bug"}, webComposerHistoryWire(msgs))
}

// TestWebComposerHistory_OrderingNewestFirst pins the recall ordering the
// frontend documents ("newest first — matches press ↑ to get the previous
// prompt"): the most recently typed web prompt is index 0, regardless of
// assistant/notice rows interleaved between human prompts.
func TestWebComposerHistory_OrderingNewestFirst(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "composer-history-order")
	require.NoError(t, err)

	createUserMsg(t, a.Messages, sess.ID, userText("first prompt"))

	noticeParams := userText("Async job call-2 (bash) finished.\n\nout")
	noticeParams.BackgroundJobNotice = true
	notice := createUserMsg(t, a.Messages, sess.ID, noticeParams)
	require.True(t, notice.BackgroundJobNotice)

	createUserMsg(t, a.Messages, sess.ID, userText("second prompt"))
	createUserMsg(t, a.Messages, sess.ID, userText("third prompt"))

	msgs, _, err := a.Messages.ListWithWatermark(ctx, sess.ID)
	require.NoError(t, err)

	require.Equal(t, []string{"third prompt", "second prompt", "first prompt"},
		webComposerHistoryWire(msgs))
}

// TestWebComposerHistory_EmptyWhenNoWebTypedPrompts is the empty-history
// edge: a session whose only user rows arrived through other channels or as
// notices must yield an EMPTY recall list — never CLI entries or notices.
func TestWebComposerHistory_EmptyWhenNoWebTypedPrompts(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "composer-history-empty")
	require.NoError(t, err)

	// CLI `rush run` prompt.
	cliParams := userText("cli only prompt")
	cliParams.Origin = message.OriginCLI
	createUserMsg(t, a.Messages, sess.ID, cliParams)

	// Async completion notice (web origin, notice + auto-resume flags) — the
	// Phase 4 shape from coordinator.notifyBackgroundJobDone.
	autoParams := userText("Async job call-3 (bash) finished.\n\nout")
	autoParams.BackgroundJobNotice = true
	autoParams.AutoResumed = true
	createUserMsg(t, a.Messages, sess.ID, autoParams)

	// SDK-originated prompt.
	sdkParams := userText("sdk only prompt")
	sdkParams.Origin = message.OriginSDK
	createUserMsg(t, a.Messages, sess.ID, sdkParams)

	// An empty-text web row (attachment-only send) — no recall entry.
	createUserMsg(t, a.Messages, sess.ID, userText("   "))

	msgs, _, err := a.Messages.ListWithWatermark(ctx, sess.ID)
	require.NoError(t, err)
	require.Empty(t, webComposerHistoryWire(msgs),
		"no web-typed entries must mean empty history, not CLI entries")

	// A truly empty session is empty too, not an error.
	empty, err := a.Sessions.Create(ctx, "composer-history-none")
	require.NoError(t, err)
	msgs, _, err = a.Messages.ListWithWatermark(ctx, empty.ID)
	require.NoError(t, err)
	require.Empty(t, webComposerHistoryWire(msgs))
}

// TestWebComposerHistory_ExcludesReportedPollution is the regression pin for
// the reported bug: after a CLI `rush run` turn and an async completion
// notice are recorded into the session, the web history endpoint's serving
// rows carry no composer-eligible prompt — both pollution classes are
// excluded even though the notice itself carries OriginWeb.
func TestWebComposerHistory_ExcludesReportedPollution(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	ctx := t.Context()

	sess, err := a.Sessions.Create(ctx, "composer-history-pollution")
	require.NoError(t, err)

	// The `rush run` path: internal/app stamps RunOverrides.Origin =
	// message.OriginCLI (internal/app/app_run_request.go) and the CLI sets it
	// at internal/cmd/run.go; the turn persists the user message with that
	// origin via agent.createUserMessage.
	cliParams := userText("write a haiku about sqlite")
	cliParams.Origin = message.OriginCLI
	cliRun := createUserMsg(t, a.Messages, sess.ID, cliParams)
	require.Equal(t, message.OriginCLI, cliRun.Origin)

	// The async completion notice recorded when the background job finished
	// (coordinator.notifyAsyncCompletion → FormatAsyncCompletion).
	noticeParams := userText("Async job call-9 (bash) finished.\n\nexit 0")
	noticeParams.BackgroundJobNotice = true
	createUserMsg(t, a.Messages, sess.ID, noticeParams)

	// Serve through the exact wire conversion handleLoadMessages uses.
	msgs, _, err := a.Messages.ListWithWatermark(ctx, sess.ID)
	require.NoError(t, err)
	rows := toMessagesWire(msgs)
	require.Len(t, rows, 2, "both rows belong in the transcript, served to the browser")

	// Revert-check: if either the Origin wire field or the client-side
	// origin/notice filter is lost, one of these rows becomes recall-eligible
	// again and the polluting text surfaces under ArrowUp.
	require.Empty(t, webComposerHistoryWire(msgs),
		"CLI-originated prompts and async completion notices must never enter web recall")
}

// TestIsHumanTyped is the table-driven pin for the predicate itself (#1056):
// every known message.Message.NoticeKind value the wake/async job machinery
// persists -- "" (ordinary finish/fail/cancel), "timeout_terminated",
// "job_stopped", "timeout_wake_only", "wake_failed", "supervision" (see
// coordinator_background.go, coordinator_wake.go, supervision.go,
// work_ledger_timeout.go for where each is stamped) -- plus an unenumerated
// future value ("session_cancel", from docs/plans/2026-09-28-async-phase4-
// spec.md's planned Phase 4 work), to prove the predicate excludes by
// NoticeKind != "" rather than by matching a fixed list of known values.
//
// Each case sets Origin=OriginWeb and leaves AutoResumed/BackgroundJobNotice
// false UNLESS the case says otherwise, specifically to isolate what each
// field contributes: supervision and timeout_wake_only notices are, in
// production, persisted via a plain context.Background() (no AutoResumed, no
// BackgroundJobNotice, no origin at all) -- NoticeKind is their ONLY
// structured marker, so a case with Origin=web and no other flags proves
// NoticeKind alone must be load-bearing, independent of Origin.
//
// Revert-check: if isHumanTyped stops checking NoticeKind (reverting to the
// pre-#1056 AutoResumed/BackgroundJobNotice/Origin-only check), the
// "supervision" and "timeout_wake_only" cases below flip from false to true.
func TestIsHumanTyped(t *testing.T) {
	t.Parallel()

	base := func() message.Message {
		return message.Message{
			ID:     "m1",
			Role:   message.User,
			Origin: message.OriginWeb,
			Parts:  []message.ContentPart{message.TextContent{Text: "hello"}},
		}
	}

	for _, tc := range []struct {
		name string
		mod  func(m *message.Message)
		want bool
	}{
		{"plain web-typed prompt", func(m *message.Message) {}, true},
		{"assistant role", func(m *message.Message) { m.Role = message.Assistant }, false},
		{"tool role", func(m *message.Message) { m.Role = message.Tool }, false},
		{"hidden", func(m *message.Message) { m.Hidden = true }, false},
		{"summary message", func(m *message.Message) { m.IsSummaryMessage = true }, false},
		{"cli origin", func(m *message.Message) { m.Origin = message.OriginCLI }, false},
		{"sdk origin", func(m *message.Message) { m.Origin = message.OriginSDK }, false},
		{"unspecified origin", func(m *message.Message) { m.Origin = message.OriginUnspecified }, false},
		{"auto-resumed", func(m *message.Message) { m.AutoResumed = true }, false},
		{"background job notice flag", func(m *message.Message) { m.BackgroundJobNotice = true }, false},
		{"both notice flags (async completion notice shape)", func(m *message.Message) {
			m.AutoResumed = true
			m.BackgroundJobNotice = true
		}, false},
		{"NoticeKind ordinary finish (empty, unaffected)", func(m *message.Message) { m.NoticeKind = "" }, true},
		{"NoticeKind timeout_terminated", func(m *message.Message) { m.NoticeKind = "timeout_terminated" }, false},
		{"NoticeKind job_stopped", func(m *message.Message) { m.NoticeKind = "job_stopped" }, false},
		{"NoticeKind timeout_wake_only, no other flags", func(m *message.Message) { m.NoticeKind = "timeout_wake_only" }, false},
		{"NoticeKind wake_failed", func(m *message.Message) { m.NoticeKind = "wake_failed" }, false},
		{"NoticeKind supervision, no other flags", func(m *message.Message) { m.NoticeKind = noticeKindSupervisionForTest }, false},
		{"NoticeKind unenumerated future value", func(m *message.Message) { m.NoticeKind = "session_cancel" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := base()
			tc.mod(&m)
			require.Equal(t, tc.want, isHumanTyped(m))
		})
	}
}

// noticeKindSupervisionForTest mirrors internal/agent's unexported
// noticeKindSupervision constant ("supervision") -- duplicated here rather
// than imported because internal/agent is not (and must not become) a
// dependency of internal/server's wire layer.
const noticeKindSupervisionForTest = "supervision"

// TestMessageWire_NoticeKindReachesTheBrowser pins the serving half of
// #1056: NoticeKind must survive toMessageWire, matching Origin's existing
// contract (TestMessageWire_OriginReachesTheBrowser above), because
// isHumanTyped's web-side TypeScript mirror (should NoticeKind ever be
// needed client-side beyond HumanTyped) has nothing to filter on otherwise.
func TestMessageWire_NoticeKindReachesTheBrowser(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"", "timeout_terminated", "job_stopped", "timeout_wake_only", "wake_failed", "supervision"} {
		t.Run("kind="+kind, func(t *testing.T) {
			t.Parallel()
			wire := toMessageWire(message.Message{
				ID:         "m1",
				Role:       message.User,
				NoticeKind: kind,
				Parts:      []message.ContentPart{message.TextContent{Text: "hello"}},
			})
			require.Equal(t, kind, wire.NoticeKind)
		})
	}
}
