package agent

import (
	"encoding/json"
	"math"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/shell"
)

// stepCallClass is what one tool call of one step WAS, judged from the call's
// own input plus its result -- never from the result's text (a failed edit is
// still a real attempt, a red herring is still a read). It is the single
// verdict the in-turn progress guard and the reaction chain guard (#1113)
// both read.
type stepCallClass int

const (
	// stepCallAct may change the world: edit/write/fs_*, bash/run_command with
	// a real command, agent/fetch, job_kill, wake*, unknown MCP tools. An
	// error in the result does not downgrade it.
	stepCallAct stepCallClass = iota
	// stepCallRead reads at least one line not covered yet in this turn.
	stepCallRead
	// stepCallReread is a view/fs_read whose every window is already covered:
	// the step observes without learning anything new.
	stepCallReread
	// stepCallWait is an ASYNC launch of a pure wait command (bash
	// sleep/echo, run_command sleep/timeout). A synchronous SDK sleep blocks
	// for real and stays act, exactly as #1113 always counted it.
	stepCallWait
	// stepCallRefused is the in-turn guard's own refusal: the tool never ran.
	stepCallRefused
	// stepCallNeutral neither advances nor launches: job_output/todos, or an
	// unknown tool name (nothing was in the set to run).
	stepCallNeutral
)

func (c stepCallClass) String() string {
	switch c {
	case stepCallAct:
		return "act"
	case stepCallRead:
		return "read"
	case stepCallReread:
		return "reread"
	case stepCallWait:
		return "wait"
	case stepCallRefused:
		return "refused"
	case stepCallNeutral:
		return "neutral"
	}
	return "unknown"
}

// readSpan is a half-open line window [start, end); end == math.MaxInt means
// "to the end of the file". Bounds come from the call's own request, so the
// tail past EOF is an acceptable over-estimate.
type readSpan struct{ start, end int }

// readCoverage maps a cleaned file path to the line windows already read in
// this turn. A nil map means "coverage is not tracked at all", which makes
// every windowed read a fresh read.
type readCoverage map[string][]readSpan

// covered reports whether sp lies entirely inside the union of the windows
// recorded for path.
func (c readCoverage) covered(path string, sp readSpan) bool {
	if sp.end <= sp.start {
		return true // an empty window reads nothing
	}
	reach := sp.start
	for _, s := range mergeSpans(c[filepath.Clean(path)]) {
		if s.end <= reach {
			continue
		}
		if s.start > reach {
			return false // a gap opens before sp.end
		}
		reach = s.end
		if reach >= sp.end {
			return true
		}
	}
	return false
}

// add records sp for path, keeping that path's windows sorted and merged.
func (c readCoverage) add(path string, sp readSpan) {
	if c == nil || sp.end <= sp.start {
		return
	}
	key := filepath.Clean(path)
	c[key] = mergeSpans(append(c[key], sp))
}

// clear drops every recorded window (an act, a message insert or a trim all
// mean "the file may have changed").
func (c readCoverage) clear() {
	for key := range c {
		delete(c, key)
	}
}

// snapshot deep-copies the coverage for a concurrent reader (a tool wrapper).
func (c readCoverage) snapshot() readCoverage {
	out := make(readCoverage, len(c))
	for key, spans := range c {
		out[key] = append(make([]readSpan, 0, len(spans)), spans...)
	}
	return out
}

// mergeSpans sorts windows by start and merges overlapping or touching ones in
// a single pass. Files are small, so a copy plus sort beats an interval tree.
func mergeSpans(spans []readSpan) []readSpan {
	if len(spans) < 2 {
		return spans
	}
	out := append(make([]readSpan, 0, len(spans)), spans...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end < out[j].end
	})
	merged := out[:1]
	for _, s := range out[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// isWaitOnlyCall reports whether call is a pure wait: a bash command whose
// every invocation is a literal sleep/echo/printf/true/: with no redirects,
// pipes or substitutions, or a run_command whose program is sleep/timeout.
// Whether that wait is an ASYNC launch (wait) or a blocking sync call (act)
// is the metadata's business, decided in classifyStepCall.
func isWaitOnlyCall(call fantasy.ToolCallContent) bool {
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

// namedSpan is one read window of one file, taken from a call's own input.
type namedSpan struct {
	path string
	span readSpan
}

// readSpans extracts the line windows a view/fs_read call asked for, one per
// item for fs_read's batch form. Any other tool, an unparsable input, or a
// fs_read item the real tool itself would reject yields no window for it --
// the guard does not punish input it cannot read.
func readSpans(call fantasy.ToolCallContent) []namedSpan {
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
		return []namedSpan{{path: params.FilePath, span: readSpan{start: params.Offset, end: params.Offset + limit}}}
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
		out := make([]namedSpan, 0, len(params.Items))
		for _, item := range params.Items {
			if span, ok := fsReadItemSpan(item.Path, item.StartLine, item.EndLine, item.Line, item.Radius); ok {
				out = append(out, namedSpan{path: item.Path, span: span})
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
func fsReadItemSpan(path string, startLine, endLine, line, radius int) (readSpan, bool) {
	if strings.TrimSpace(path) == "" {
		return readSpan{}, false
	}
	switch {
	case startLine != 0 || endLine != 0:
		if startLine < 1 || (endLine != 0 && endLine < startLine) {
			return readSpan{}, false
		}
		end := endLine
		if end == 0 {
			end = math.MaxInt
		}
		return readSpan{start: startLine - 1, end: end}, true
	case line != 0 || radius != 0:
		if line < 1 || radius < 0 {
			return readSpan{}, false
		}
		start := line - 1 - radius
		if start < 0 {
			start = 0
		}
		return readSpan{start: start, end: line + radius}, true
	default:
		return readSpan{start: 0, end: math.MaxInt}, true
	}
}

// progressGuardMetadata is the ClientMetadata tag the in-turn guard stamps on
// a result it refused (step 2 of the plan writes it): the tool never ran, so
// the step neither progressed nor launched anything.
type progressGuardMetadata struct {
	ProgressGuard struct {
		Refused string `json:"refused"`
	} `json:"progress_guard"`
}

// guardRefused reports whether result carries the guard's refusal tag.
func guardRefused(result fantasy.ToolResultContent) bool {
	var guard progressGuardMetadata
	if json.Unmarshal([]byte(result.ClientMetadata), &guard) != nil {
		return false
	}
	return guard.ProgressGuard.Refused != ""
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

// stepClassifyCtx carries the turn-scoped state a classification may consult.
// A zero ctx (nil seen) is the #1113 configuration: reread detection is off
// and every windowed read counts as a fresh read.
type stepClassifyCtx struct {
	seen readCoverage // nil: reread detection impossible; view/fs_read classify as read
}

// classifyStepCall is the ONE place that decides what a call was. The order
// matters: a guard refusal outranks everything (the tool never ran), then the
// always-neutral tools, then the unknown-tool error, then the wait-only
// launch gated on async metadata, then the windowed reads against coverage,
// then the restricted-action table, and everything else is act.
func classifyStepCall(call fantasy.ToolCallContent, result fantasy.ToolResultContent, cx stepClassifyCtx) stepCallClass {
	if guardRefused(result) {
		return stepCallRefused
	}
	if _, neutral := chainNeutralTools[call.ToolName]; neutral {
		return stepCallNeutral
	}
	if unknownToolResult(result) {
		return stepCallNeutral
	}
	if isWaitOnlyCall(call) {
		var meta asyncToolMetadata
		_ = json.Unmarshal([]byte(result.ClientMetadata), &meta)
		if meta.Async || meta.Inline {
			return stepCallWait
		}
		// no async metadata: a synchronous sleep really blocks (SDK) -- act
	}
	switch call.ToolName {
	case tools.ViewToolName, tools.FSReadToolName:
		spans := readSpans(call)
		if len(spans) == 0 {
			return stepCallRead // unreadable input: do not punish the model
		}
		if cx.seen == nil {
			return stepCallRead
		}
		for _, named := range spans {
			if !cx.seen.covered(named.path, named.span) {
				return stepCallRead
			}
		}
		return stepCallReread
	}
	if action := restrictedToolActions[call.ToolName]; action == "read" || action == "list" {
		return stepCallRead
	}
	return stepCallAct
}
