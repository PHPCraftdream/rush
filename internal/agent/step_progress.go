package agent

import (
	"path/filepath"
	"sort"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
)

// stepCallClass and its constants alias the leaf classifier's verdict type
// (runtime/exec/turnstep) for the root guard, reaction chain and their tests.
type stepCallClass = turnstep.StepCallClass

const (
	stepCallAct     = turnstep.StepCallAct
	stepCallRead    = turnstep.StepCallRead
	stepCallReread  = turnstep.StepCallReread
	stepCallWait    = turnstep.StepCallWait
	stepCallRefused = turnstep.StepCallRefused
	stepCallNeutral = turnstep.StepCallNeutral
	stepCallNoWrite = turnstep.StepCallNoWrite
)

// readSpan is a half-open line window [start, end); end == math.MaxInt means
// "to the end of the file". Bounds come from the call's own request, so the
// tail past EOF is an acceptable over-estimate.
type readSpan struct{ start, end int }

func toLeafSpan(sp readSpan) turnstep.ReadSpan {
	return turnstep.ReadSpan{Start: sp.start, End: sp.end}
}

func fromLeafSpan(sp turnstep.ReadSpan) readSpan { return readSpan{start: sp.Start, end: sp.End} }

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

func isWaitOnlyCall(call fantasy.ToolCallContent) bool {
	return turnstep.IsWaitOnlyCall(call)
}

// namedSpan is one read window of one file, taken from a call's own input.
type namedSpan struct {
	path string
	span readSpan
}

// readSpans extracts the line windows a view/fs_read call asked for, one per
// item for fs_read's batch form. Any other tool, an unparsable input, or a
// fs_read item the real tool itself would reject yields no window for it --
// the guard does not punish input it cannot read. The parse itself lives in
// the leaf package (turnstep.ReadSpans).
func readSpans(call fantasy.ToolCallContent) []namedSpan {
	leaf := turnstep.ReadSpans(call)
	out := make([]namedSpan, 0, len(leaf))
	for _, named := range leaf {
		out = append(out, namedSpan{path: named.Path, span: fromLeafSpan(named.Span)})
	}
	return out
}

func asyncCapRefused(result fantasy.ToolResultContent) bool {
	return turnstep.AsyncCapRefused(result)
}

// stepClassifyCtx carries the turn-scoped state a classification may consult.
// A zero ctx (nil seen) is the #1113 configuration: reread detection is off
// and every windowed read counts as a fresh read.
type stepClassifyCtx struct {
	seen readCoverage // nil: reread detection impossible; view/fs_read classify as read
}

// seam hands the coverage and the restricted-action table to the leaf
// classifier through its use-point seams; the coverage callback runs at the
// classifier's original readSpans parse site and reads an untracked map
// (nil seen) as fresh windows.
func (cx stepClassifyCtx) seam() turnstep.StepClassifyCtx {
	return turnstep.StepClassifyCtx{
		RestrictedAction: func(toolName string) string { action, _ := toolwrap.RestrictedToolAction(toolName); return action },
		CoveredWindows: func(call fantasy.ToolCallContent) (int, bool) {
			spans := turnstep.ReadSpans(call)
			if len(spans) == 0 {
				return 0, false
			}
			for _, named := range spans {
				if cx.seen == nil || !cx.seen.covered(named.Path, fromLeafSpan(named.Span)) {
					return len(spans), false
				}
			}
			return len(spans), true
		},
	}
}

// classifyStepCall delegates the verdict to the leaf classifier
// (runtime/exec/turnstep.ClassifyStepCall) against this root ctx's seams.
func classifyStepCall(call fantasy.ToolCallContent, result fantasy.ToolResultContent, cx stepClassifyCtx) stepCallClass {
	return turnstep.ClassifyStepCall(call, result, cx.seam())
}
