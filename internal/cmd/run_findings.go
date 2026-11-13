// Fork-only file: the findings block appended to a new run's prompt by
// `rush run --findings-from`. Reads another run's envelope (resolved
// and loaded via envelope.go) and formats its review findings so the
// model explicitly knows what a previous review concluded.

package cmd

import (
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/app"
)

// findingsBlockTitle heads the block appended by --findings-from.
const findingsBlockTitle = "## Findings from previous review"

// formatFindingsBlock renders an envelope's review findings as a
// deterministic plain-text block: verdict, review text and warnings.
// When none are recorded the body says so explicitly, because the
// block is always appended so the model knows the reference run
// produced no findings. No color, no styling.
func formatFindingsBlock(env *app.RunResult) string {
	var b strings.Builder
	b.WriteString(findingsBlockTitle)
	b.WriteString("\n\n")
	if env.ReviewVerdict != "" {
		fmt.Fprintf(&b, "verdict: %s\n", env.ReviewVerdict)
	}
	if env.Review != "" {
		if env.ReviewVerdict != "" {
			b.WriteString("\n")
		}
		b.WriteString(env.Review)
		b.WriteString("\n")
	}
	if len(env.Warnings) > 0 {
		b.WriteString("\nwarnings:\n")
		for _, warn := range env.Warnings {
			fmt.Fprintf(&b, "- %s\n", warn)
		}
	}
	if env.ReviewVerdict == "" && env.Review == "" && len(env.Warnings) == 0 {
		b.WriteString("(none recorded in the envelope).\n")
	}
	return b.String()
}

// applyFindingsFrom appends the findings block from an envelope to the
// prompt. An empty ref means the flag was not passed and the prompt is
// returned unchanged: `rush run` behaves exactly as before. A missing
// or invalid envelope returns an error BEFORE the run starts, so a bad
// reference never wastes a live turn.
func applyFindingsFrom(prompt, ref string) (string, error) {
	if ref == "" {
		return prompt, nil
	}
	path, err := resolveEnvelopePath(ref)
	if err != nil {
		return "", err
	}
	_, env, _, err := loadEnvelope(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + formatFindingsBlock(env), nil
}
