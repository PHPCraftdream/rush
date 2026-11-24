// Fork-only file: the `rush result` command. Prints a run's result
// envelope (see app.RunResult) as readable text, raw JSON, the review
// section, or a single field. Pure file reads: no database, session
// store or network, nothing written to disk.

package reports

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/spf13/cobra"
)

var resultCmd = &cobra.Command{
	Use:   "result <session-id|path>",
	Short: "Print a run's result envelope as readable text, JSON, review, or a single field",
	Long: `Print a run's result envelope as readable text, JSON, the review, or one field.

The argument is either a session id — looked up as
<main-checkout>/.rush/stdin/<id>.out via ` + "`git rev-parse --git-common-dir`" + `,
so it resolves from inside a linked worktree too — or a direct path to
an envelope file. Use --history with a session id to list timestamped
archived envelopes with their exit reason and review verdict.

By default the envelope is rendered as readable text: exit reason,
error, final text, review verdict and review, warnings, brief usage
and duration. Output is always raw UTF-8 bytes on stdout regardless of
the Windows console code page. Exit code 0 when the envelope was read;
non-zero when it was not found or is not valid JSON.

` + "`rush result`" + ` needs no database, session store or network and writes
nothing to disk. It pairs with ` + "`rush run --worktree`" + ` (which stores
the latest envelope at <main>/.rush/stdin/<session>.out and archives
previous runs there) and with
` + "`rush run --findings-from`" + ` (which feeds the latest envelope back into
a new run).`,
	Example: `  rush result s1-review                    # human-readable text
  rush result s1-review --history           # timestamped archived runs
  rush result s1-review --review             # verdict + review only
  rush result s1-review --field final_text   # raw final_text for scripting
  rush result s1-review --json | jq .exit_reason
  rush result /abs/path/envelope.out         # explicit file`,
	Args:         cobra.ExactArgs(1),
	RunE:         runResultCmd,
	SilenceUsage: true,
}

// addResultFlags registers result's flags. Kept as a named helper so
// tests can build a fresh command with the same flag set.
func addResultFlags(c *cobra.Command) {
	c.Flags().Bool("json", false, "Write the envelope bytes to stdout exactly as stored (no re-formatting, no re-encoding).")
	c.Flags().Bool("review", false, "Write only the reviewer verdict and the review text (nothing when the envelope has neither).")
	c.Flags().String("field", "", "Write one envelope field's value; strings are written raw (no quotes), other types as JSON. Fails when the field is absent.")
	c.Flags().Bool("history", false, "List timestamped archived envelopes for this session with exit reason and review verdict.")
}

func runResultCmd(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	asReview, _ := cmd.Flags().GetBool("review")
	history, _ := cmd.Flags().GetBool("history")
	field, _ := cmd.Flags().GetString("field")
	set := 0
	for _, b := range []bool{asJSON, asReview, history} {
		if b {
			set++
		}
	}
	if field != "" {
		set++
	}
	if set > 1 {
		return fmt.Errorf("--json, --review, --history and --field are mutually exclusive")
	}
	out := cmd.OutOrStdout()
	if history {
		if strings.ContainsAny(args[0], `/\\*?[]`) || args[0] == "." || args[0] == ".." {
			return fmt.Errorf("--history requires a session id, not an envelope path")
		}
		return writeResultHistory(out, args[0])
	}
	path, err := cliout.ResolveEnvelopePath(args[0])
	if err != nil {
		return err
	}
	raw, env, m, err := cliout.LoadEnvelope(path)
	if err != nil {
		return err
	}
	switch {
	case asJSON:
		// Bytes pass through unchanged: valid UTF-8 in the file stays
		// valid UTF-8 on stdout on every platform, including Windows
		// consoles configured with a legacy code page (cp1252 etc.).
		// Go performs no ANSI/OEM transcoding on os.Stdout.
		_, err := out.Write(raw)
		return err
	case asReview:
		if env.ReviewVerdict != "" {
			fmt.Fprintf(out, "verdict: %s\n", env.ReviewVerdict)
		}
		if env.Review != "" {
			fmt.Fprintf(out, "%s\n", env.Review)
		}
		return nil
	case field != "":
		v, ok := m[field]
		if !ok {
			return fmt.Errorf("field %q not found in envelope %s", field, path)
		}
		if s, isStr := v.(string); isStr {
			// Raw string value, no quoting: scripts slice this directly.
			fmt.Fprintf(out, "%s\n", s)
			return nil
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("cannot render field %q of envelope %s: %w", field, path, err)
		}
		fmt.Fprintf(out, "%s\n", b)
		return nil
	default:
		writeResultText(out, env)
		return nil
	}
}

func writeResultHistory(w io.Writer, sessionID string) error {
	root, err := cliout.MainCheckoutRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, ".rush", "stdin")
	return writeResultHistoryFromDir(w, dir, sessionID)
}

func writeResultHistoryFromDir(w io.Writer, dir, sessionID string) error {
	matches, err := filepath.Glob(filepath.Join(dir, sessionID+".*.out"))
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("no archived envelopes for session %q", sessionID)
	}
	sort.Strings(matches)
	found := false
	for _, path := range matches {
		stamp := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), sessionID+"."), ".out")
		if !isResultHistoryTimestamp(stamp) {
			continue
		}
		found = true
		_, env, _, err := cliout.LoadEnvelope(path)
		if err != nil {
			fmt.Fprintf(w, "%s\tunreadable\n", stamp)
			continue
		}
		fmt.Fprintf(w, "%s\texit_reason=%s\treview_verdict=%s\n", stamp, env.ExitReason, env.ReviewVerdict)
	}
	if !found {
		return fmt.Errorf("no archived envelopes for session %q", sessionID)
	}
	return nil
}

func isResultHistoryTimestamp(stamp string) bool {
	parsed, err := time.Parse("20060102T150405.000000000Z", stamp)
	return err == nil && parsed.UTC().Format("20060102T150405.000000000Z") == stamp
}

// All writes are raw UTF-8 bytes via fmt.Fprintf: no encoding layer
// exists between the envelope's strings and stdout, so Cyrillic and
// any other Unicode survive on Windows regardless of the console code
// page.
func writeResultText(w io.Writer, env *app.RunResult) {
	var buf bytes.Buffer
	if env.SessionID != "" {
		fmt.Fprintf(&buf, "session: %s\n", env.SessionID)
	}
	fmt.Fprintf(&buf, "exit reason: %s\n", env.ExitReason)
	if env.Error != "" {
		fmt.Fprintf(&buf, "error: %s\n", env.Error)
	}
	buf.WriteString("\nfinal text:\n")
	if body := strings.TrimSpace(env.FinalText); body != "" {
		buf.WriteString(env.FinalText)
		if !strings.HasSuffix(env.FinalText, "\n") {
			buf.WriteString("\n")
		}
	} else {
		buf.WriteString("(empty)\n")
	}
	if env.ReviewVerdict != "" {
		buf.WriteString("\n")
		fmt.Fprintf(&buf, "review verdict: %s\n", env.ReviewVerdict)
	}
	if env.Review != "" {
		buf.WriteString("review:\n")
		buf.WriteString(env.Review)
		if !strings.HasSuffix(env.Review, "\n") {
			buf.WriteString("\n")
		}
	}
	if len(env.Warnings) > 0 {
		buf.WriteString("\nwarnings:\n")
		for _, warn := range env.Warnings {
			fmt.Fprintf(&buf, "- %s\n", warn)
		}
	}
	buf.WriteString("\n")
	fmt.Fprintf(&buf, "usage: %d tokens, $%s this run",
		env.Usage.DeltaTokens,
		strconv.FormatFloat(env.Usage.DeltaCostUSD, 'f', -1, 64))
	if s := env.Usage.Session; s != nil {
		fmt.Fprintf(&buf, ", $%s / %d tokens session total",
			strconv.FormatFloat(s.CostUSD, 'f', -1, 64), s.TotalTokens)
	}
	humanized := (time.Duration(env.DurationMs) * time.Millisecond).String()
	fmt.Fprintf(&buf, " duration: %s\n", humanized)

	// The payload is written as raw UTF-8 bytes; valid UTF-8 in the
	// envelope stays valid UTF-8 on stdout on every platform, no
	// code-page conversion involved.
	if !utf8.Valid(buf.Bytes()) {
		// Defensive: cannot happen with JSON-decoded strings, but a
		// non-UTF-8 payload must never be silently transcoded.
		w.Write([]byte("(envelope contains invalid UTF-8)\n"))
		return
	}
	w.Write(buf.Bytes())
}

// NewResultCmd returns the rush resultCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var newResultCmdOnce sync.Once

func NewResultCmd() *cobra.Command {
	newResultCmdOnce.Do(func() {
		addResultFlags(resultCmd)
	})
	return cmdkit.DeepFreshCmd(resultCmd)
}
