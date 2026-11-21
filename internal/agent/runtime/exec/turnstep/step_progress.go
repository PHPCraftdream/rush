package turnstep

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
)

// editMadeNoWrite reports whether an edit/multi_edit result carries the
// adapter's no-write tag: the tool ran, matched (or failed to match) its
// targets, and provably changed nothing.
func editMadeNoWrite(call fantasy.ToolCallContent, result fantasy.ToolResultContent) bool {
	if (call.ToolName != tools.EditToolName && call.ToolName != tools.MultiEditToolName) || result.Result == nil || result.Result.GetType() != fantasy.ToolResultContentTypeError {
		return false
	}
	var metadata tools.EditEffectMetadata
	if json.Unmarshal([]byte(result.ClientMetadata), &metadata) != nil || !metadata.EditEffect.NoWrite {
		return false
	}
	switch metadata.EditEffect.Reason {
	case "missing_old", "equal_content", "multiple_matches":
		return call.ToolName == tools.EditToolName
	case "unchanged_content":
		return call.ToolName == tools.MultiEditToolName
	default:
		return false
	}
}

// IsWaitOnlyCall reports whether call is a pure wait: a bash command whose
// every invocation is a literal sleep/echo/printf/true/: with no redirects,
// pipes or substitutions, or a run_command whose program is sleep/timeout.
// Whether that wait is an ASYNC launch (wait) or a blocking sync call (act)
// is the metadata's business, decided by the step classifier.
func IsWaitOnlyCall(call fantasy.ToolCallContent) bool {
	switch call.ToolName {
	case tools.BashToolName:
		var params struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return false
		}
		return shell.IsNoOpCommand(params.Command)
	case tools.RunCommandToolName:
		var params struct {
			Program string `json:"program"`
		}
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return false
		}
		return params.Program == "sleep" || params.Program == "timeout"
	default:
		return false
	}
}

// chainNeutralTools are tool calls that neither advance the session's work
// nor launch anything: reading a background job's buffered output and
// managing the todo list. Every other tool (edit/write/view/grep and
// friends, a bash/run_command call with substance, a delegation, a stop
// control) is progress for the reaction chain guard (#1113).
// batch_status/batch_output are the detached-output polling reads -- neutral
// the same way the deleted job_output polling tool was.
var chainNeutralTools = map[string]struct{}{
	tools.TodosToolName: {},
	"batch_status":      {},
	"batch_output":      {},
}

// StepCallClass is what one tool call of one step WAS, judged from the call's
// own input plus structured result metadata, never inferred from error text.
// Untagged errors remain potentially effectful. It is the single
// verdict the in-turn progress guard and the reaction chain guard (#1113)
// both read.
type StepCallClass int

const (
	// StepCallAct may change the world: edit/write/fs_*, bash/run_command with
	// a real command, agent/fetch, a stop control, wake*, unknown MCP tools.
	// An error in the result does not downgrade it.
	StepCallAct StepCallClass = iota
	// StepCallRead reads at least one line not covered yet in this turn.
	StepCallRead
	// StepCallReread is a view/fs_read whose every window is already covered:
	// the step observes without learning anything new.
	StepCallReread
	// StepCallWait is an ASYNC launch of a pure wait command (bash
	// sleep/echo, run_command sleep/timeout). A synchronous SDK sleep blocks
	// for real and stays act, exactly as #1113 always counted it.
	StepCallWait
	// StepCallRefused is the in-turn guard's own refusal: the tool never ran.
	StepCallRefused
	// StepCallNeutral neither advances nor launches: batch_output/todos, or
	// an unknown tool name (nothing was in the set to run).
	StepCallNeutral
	// StepCallNoWrite is a native edit proven to have made no file write.
	StepCallNoWrite
)

func (c StepCallClass) String() string {
	switch c {
	case StepCallAct:
		return "act"
	case StepCallRead:
		return "read"
	case StepCallReread:
		return "reread"
	case StepCallWait:
		return "wait"
	case StepCallRefused:
		return "refused"
	case StepCallNeutral:
		return "neutral"
	case StepCallNoWrite:
		return "no_write"
	}
	return "unknown"
}

// waitOnlyMetadata is the async/inline half of the started-response
// ClientMetadata tag the wait branch of the classifier reads (the tag's
// producer keeps the full shape in the root package).
type waitOnlyMetadata struct {
	Async  bool `json:"async"`
	Inline bool `json:"inline"`
}

// StepClassifyCtx carries the turn-scoped state a classification may consult.
// The zero value is the #1113 configuration: reread detection is off and every
// windowed read counts as a fresh read. The root owns the coverage map and the
// restricted-action table and hands them over through these seams.
type StepClassifyCtx struct {
	// CoveredWindows reports how many readable line windows the view/fs_read
	// call asks for and whether every one is already covered in this turn.
	// It is consulted at the original readSpans parse site; zero windows or
	// an untracked coverage map (nil seen) read as fresh reads.
	CoveredWindows func(call fantasy.ToolCallContent) (windows int, allCovered bool)
	// RestrictedAction returns the restricted-tool action of a tool name
	// ("" when it is not in the table); the classifier treats "read"/"list"
	// as a windowless read.
	RestrictedAction func(toolName string) string
}

// ClassifyStepCall is the ONE place that decides what a call was. The order
// matters: a refusal outranks everything (the tool never ran) -- the guard's
// own tag first, then the async-cap tag, which is a different seam of the same
// "nothing executed" verdict and is never masked by the guard's -- then the
// always-neutral tools, then the unknown-tool error, then the wait-only
// launch gated on async metadata, then the windowed reads against coverage,
// then the restricted-action table, and everything else is act.
func ClassifyStepCall(call fantasy.ToolCallContent, result fantasy.ToolResultContent, cx StepClassifyCtx) StepCallClass {
	if guardRefused(result) {
		return StepCallRefused
	}
	if AsyncCapRefused(result) {
		return StepCallRefused
	}
	if _, neutral := chainNeutralTools[call.ToolName]; neutral {
		return StepCallNeutral
	}
	if unknownToolResult(result) {
		return StepCallNeutral
	}
	if editMadeNoWrite(call, result) {
		return StepCallNoWrite
	}
	if IsWaitOnlyCall(call) {
		var meta waitOnlyMetadata
		_ = json.Unmarshal([]byte(result.ClientMetadata), &meta)
		if meta.Async || meta.Inline {
			return StepCallWait
		}
		// no async metadata: a synchronous sleep really blocks (SDK) -- act
	}
	switch call.ToolName {
	case tools.ViewToolName, tools.FSReadToolName:
		windows, allCovered := cx.CoveredWindows(call)
		if windows == 0 {
			return StepCallRead // unreadable input: do not punish the model
		}
		if allCovered {
			return StepCallReread
		}
		return StepCallRead
	}
	if action := cx.RestrictedAction(call.ToolName); action == "read" || action == "list" {
		return StepCallRead
	}
	return StepCallAct
}

// progressGuardMetadata is the ClientMetadata tag the in-turn guard stamps on
// a result it refused (step 2 of the plan writes it): the tool never ran, so
// the step neither progressed nor launched anything.
type progressGuardMetadata struct {
	ProgressGuard struct {
		Refused string `json:"refused"`
	} `json:"progress_guard"`
}

// ProgressGuardMetadataJSON marshals the ClientMetadata tag stamped on a
// refusal; the classifier reads it to mark the call as refused (the tool never
// ran, so it neither progressed nor launched anything).
func ProgressGuardMetadataJSON(refused string) string {
	var meta progressGuardMetadata
	meta.ProgressGuard.Refused = refused
	data, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(data)
}

// guardRefused reports whether result carries the guard's refusal tag.
func guardRefused(result fantasy.ToolResultContent) bool {
	var guard progressGuardMetadata
	if json.Unmarshal([]byte(result.ClientMetadata), &guard) != nil {
		return false
	}
	return guard.ProgressGuard.Refused != ""
}

// AsyncCapRefused reports whether result carries the async-cap tag of
// the deleted async-tool cap branch: admission refused the launch, so the tool
// never ran and took no claim -- the tool's own refusal, not the guard's.
func AsyncCapRefused(result fantasy.ToolResultContent) bool {
	var meta batchmeta.AsyncCapMetadata
	if json.Unmarshal([]byte(result.ClientMetadata), &meta) != nil {
		return false
	}
	return meta.AsyncCap.Running > 0 || meta.AsyncCap.Limit > 0
}

// unknownToolResultPrefix is fantasy's bare answer to a call whose tool name
// is not in the step's set: nothing executed, so the call is neutral.
const unknownToolResultPrefix = "tool not found:"

// unknownToolResult reports whether result is that bare unknown-tool error.
func unknownToolResult(result fantasy.ToolResultContent) bool {
	if result.Result == nil ||
		result.Result.GetType() != fantasy.ToolResultContentTypeError {
		return false
	}
	errResult, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result)
	if !ok || errResult.Error == nil {
		return false
	}
	return strings.Contains(errResult.Error.Error(), unknownToolResultPrefix)
}

// The in-turn progress guard (§2 of docs/plans/2026-10-01-in-turn-progress-guard.md).
// #1113 counts a reaction CHAIN across turns; nothing watched what happens
// INSIDE one turn, where the model burned 56–520 steps polling sleeps and
// re-reading the same windows (A22/A23). This guard is a second, independent
// law: a per-turn streak of steps that made no progress, a soft refusal at S
// and a hard stop at H.
const (
	// InTurnRefuseAfter (S): from this many consecutive no-progress steps a
	// wait launch or an already-covered re-read is refused instead of run.
	InTurnRefuseAfter = 2
	// InTurnStopAfter (H): the turn is finished by the guard once the streak
	// reaches this length.
	InTurnStopAfter = 5
)

// InTurnGuardStopTitle opens the assistant finish message of a turn the guard
// stopped. UI/CLI recognize it through IsInTurnGuardStop, exactly as the run
// loop recognizes a coordinator continuation prompt.
const InTurnGuardStopTitle = "Stopped: no progress in this turn"

// IsInTurnGuardStop reports whether title was written by the in-turn progress
// guard (see InTurnGuardStopTitle).
func IsInTurnGuardStop(title string) bool {
	return strings.HasPrefix(title, InTurnGuardStopTitle)
}

// RefuseWait is the wait-leg verdict (§3's table): only an ASYNC launch waits
// (a synchronous SDK sleep really blocks, so it stays act), and it is refused
// either because the session has its own undelivered work — whose completion
// already wakes it — or because the streak is past S.
func RefuseWait(streak int, ownWork, async bool) bool {
	return async && (ownWork || streak >= InTurnRefuseAfter)
}

// RefuseReread is the re-read leg verdict: every window already returned in
// this turn is in the model's context, so asking for it again learns nothing.
func RefuseReread(streak int) bool {
	return streak >= InTurnRefuseAfter
}

// NextStreak folds one step's class into the streak (§1.1): progress resets it,
// a wait/re-read/refusal/no-write extends it, a neutral step (`todos`, `batch_output`,
// an unknown tool) leaves it alone.
func NextStreak(streak int, class StepCallClass) int {
	switch class {
	case StepCallAct, StepCallRead:
		return 0
	case StepCallReread, StepCallWait, StepCallRefused, StepCallNoWrite:
		return streak + 1
	default:
		return streak
	}
}

// StopAfter reports whether the streak is long enough for the guard to end the
// turn (§2.3).
func StopAfter(streak int) bool {
	return streak >= InTurnStopAfter
}

// ClassOfStep ranks one step's calls into the single class the streak is
// computed from: act/read (progress) outrank any no-progress call, which
// outranks neutral. A step that mixes a refusal with a real read still
// progressed (§1.4 line 13).
func ClassOfStep(classes []StepCallClass) StepCallClass {
	acc := StepCallNeutral
	for _, class := range classes {
		switch class {
		case StepCallAct, StepCallRead:
			return StepCallAct
		case StepCallReread, StepCallWait, StepCallRefused, StepCallNoWrite:
			if acc == StepCallNeutral {
				acc = class
			}
		}
	}
	return acc
}

// HasClass reports whether any of classes is want.
func HasClass(classes []StepCallClass, want StepCallClass) bool {
	for _, class := range classes {
		if class == want {
			return true
		}
	}
	return false
}

// ReadSpan is a half-open line window [start, end); end == math.MaxInt means
// "to the end of the file". Bounds come from the call's own request, so the
// tail past EOF is an acceptable over-estimate.
type ReadSpan struct{ Start, End int }

// NamedSpan is one read window of one file, taken from a call's own input.
type NamedSpan struct {
	Path string
	Span ReadSpan
}

// ReadSpans extracts the line windows a view/fs_read call asked for, one per
// item for fs_read's batch form. Any other tool, an unparsable input, or a
// fs_read item the real tool itself would reject yields no window for it --
// the guard does not punish input it cannot read.
func ReadSpans(call fantasy.ToolCallContent) []NamedSpan {
	switch call.ToolName {
	case tools.ViewToolName:
		var params struct {
			FilePath string `json:"file_path"`
			Offset   int    `json:"offset"`
			Limit    int    `json:"limit"`
		}
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return nil
		}
		limit := params.Limit
		if limit <= 0 {
			limit = tools.DefaultReadLimit // view.go's own default
		}
		return []NamedSpan{{Path: params.FilePath, Span: ReadSpan{Start: params.Offset, End: params.Offset + limit}}}
	case tools.FSReadToolName:
		var params struct {
			Items []struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
				Line      int    `json:"line"`
				Radius    int    `json:"radius"`
			} `json:"items"`
		}
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return nil
		}
		out := make([]NamedSpan, 0, len(params.Items))
		for _, item := range params.Items {
			if span, ok := fsReadItemSpan(item.Path, item.StartLine, item.EndLine, item.Line, item.Radius); ok {
				out = append(out, NamedSpan{Path: item.Path, Span: span})
			}
		}
		return out
	default:
		return nil
	}
}

// fsReadItemSpan mirrors fs_read.go's fsReadWindowOf addressing modes: a
// 1-based inclusive start_line/end_line range (end_line 0 reads to EOF), a
// center line with a radius, or the whole file when no bound is given. An
// item that mixes both modes, or carries a bound the tool rejects, has no
// span -- that call fails before it reads anything.
func fsReadItemSpan(path string, startLine, endLine, line, radius int) (ReadSpan, bool) {
	if strings.TrimSpace(path) == "" {
		return ReadSpan{}, false
	}
	switch {
	case startLine != 0 || endLine != 0:
		if startLine < 1 || (endLine != 0 && endLine < startLine) {
			return ReadSpan{}, false
		}
		end := endLine
		if end == 0 {
			end = math.MaxInt
		}
		return ReadSpan{Start: startLine - 1, End: end}, true
	case line != 0 || radius != 0:
		if line < 1 || radius < 0 {
			return ReadSpan{}, false
		}
		start := line - 1 - radius
		if start < 0 {
			start = 0
		}
		return ReadSpan{Start: start, End: line + radius}, true
	default:
		return ReadSpan{Start: 0, End: math.MaxInt}, true
	}
}

// StepGuard is the immutable snapshot of one step's worth of guard state,
// built BEFORE the assistant message and handed to the tool wrappers of that
// step. Parallel calls of one step read one value — no lock, no shared
// mutable state (§1.3).
type StepGuard struct {
	Streak               int
	OwnWork, Async       bool
	CanEdit, CanDelegate bool
}

// TurnProgress is the guard's per-turn state (§1.3): what a finished turn
// counted, for the finish text.
type TurnProgress struct {
	Streak   int
	Waits    int // wait calls, for the finish text
	Rereads  int // re-read calls, for the finish text
	NoWrites int // native edit/multiedit calls proven not to write
	Stopped  bool
	Guard    StepGuard
}

// guardStopHint warns, on the last free step, that the next no-progress step
// ends the turn (§2.2). It closes the refusal text: the model reads it after
// the instruction it must follow, not before.
func guardStopHint(streak int) string {
	if streak == InTurnStopAfter-1 {
		return " If this step makes no progress, rush ends the turn now."
	}
	return ""
}

// waitCommandLabel names the refused command as the model wrote it.
func waitCommandLabel(call fantasy.ToolCall) string {
	switch call.Name {
	case tools.BashToolName:
		var params struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Input), &params) == nil && strings.TrimSpace(params.Command) != "" {
			return "`" + strings.TrimSpace(params.Command) + "`"
		}
	case tools.RunCommandToolName:
		var params struct {
			Program string `json:"program"`
		}
		if json.Unmarshal([]byte(call.Input), &params) == nil && strings.TrimSpace(params.Program) != "" {
			return "`run_command " + strings.TrimSpace(params.Program) + "`"
		}
	}
	return "`" + call.Name + "`"
}

// RefuseWaitText is §2.2's wait refusal: the command never waits (it starts as
// a background job and returns at once), so the model must end its turn
// WITHOUT a tool call — a no-op tool call is what keeps the turn going.
func RefuseWaitText(g StepGuard, call fantasy.ToolCall) string {
	text := fmt.Sprintf("Refused: %s does not wait — commands here start as background jobs and return at once. "+
		"Your running job/delegation reports by itself as a session message and starts your next turn. "+
		"End your turn now: reply with a short status and NO tool call — "+
		"a no-op tool call keeps the turn going.", waitCommandLabel(call))
	if !g.OwnWork {
		text += fmt.Sprintf(" ...and your last %d steps made no progress. To wait for time, call `wakein` and end your turn; "+
			"otherwise continue the task or end your turn with no tool call.", g.Streak)
	}
	return text + guardStopHint(g.Streak)
}

// RefuseRereadText is §2.2's re-read refusal, naming the lines so the model
// can see what it already holds. In orchestrator mode (a set without the edit
// tools but with `agent`) it says how to get the change made instead.
func RefuseRereadText(g StepGuard, first NamedSpan) string {
	text := fmt.Sprintf("Refused: lines %s of %s were already returned in this turn and are in your context. "+
		"Act on them or end your turn. "+
		"If you are waiting for a running job to write this file, end your turn — its completion wakes you.",
		spanLines(first.Span), "`"+first.Path+"`")
	if !g.CanEdit && g.CanDelegate {
		text += " This session cannot edit files (orchestrator mode): delegate the change with the `agent` tool — " +
			"file path, the exact change, how to verify it."
	}
	return text + guardStopHint(g.Streak)
}

// spanLines renders a read window as the 1-based inclusive lines it covers.
func spanLines(sp ReadSpan) string {
	if sp.End <= sp.Start {
		return "the whole file"
	}
	if sp.End == math.MaxInt {
		return fmt.Sprintf("%d–the end of the file", sp.Start+1)
	}
	return fmt.Sprintf("%d–%d", sp.Start+1, sp.End)
}

// GuardStopDetails is the assistant finish details of a guard-stopped turn
// (§2.3): what it counted, whether the session had its own work, and what the
// next turn should do.
func GuardStopDetails(p TurnProgress) string {
	details := fmt.Sprintf("The in-turn progress guard stopped this turn: %d consecutive steps with no progress, "+
		"%d wait call(s) and %d already-read window(s). ", p.Streak, p.Waits, p.Rereads)
	if p.NoWrites > 0 {
		details += fmt.Sprintf("%d native edit/multiedit call(s) made no file content write. ", p.NoWrites)
	}
	if p.Guard.OwnWork {
		details += "Your own job/delegation was running; its completion reports by itself and starts your next turn. "
	} else {
		details += "No job of this session was running. "
	}
	if p.Guard.CanDelegate && !p.Guard.CanEdit {
		details += "This session cannot edit files (orchestrator mode): delegate the remaining change with the `agent` tool. "
	}
	return details + "End the turn with a short status and NO tool call."
}
