package cmd

// `rush logs summary` -- a strictly read-only summary over one or more rush
// log files: what warned, what failed, which processes started, and where the
// log went quiet.
//
// The command never writes: every named file is opened for reading only, no
// log is written, truncated, rotated or removed (that is `logs prune`), no
// data directory is created and no database is opened. The default log file
// is resolved with config.ResolveDataDirectory rather than config.Load, so no
// configuration is persisted on the way in -- the same resolution
// `sessions audit` uses.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

// defaultSilenceGap is how much quiet between two consecutive timestamped
// lines counts as "nothing was logged for this long".
const defaultSilenceGap = 15 * time.Minute

var logsSummaryCmd = &cobra.Command{
	Use:   "summary",
	Short: "Summarize rush logs (strictly read-only)",
	Long: `Summarize one or more rush log files: what warned, what failed, which
processes started, and where the log went quiet.

Read-only: every named file is opened for reading and nothing else. No log
file is written, truncated, rotated or removed, no data directory is created
and no database is opened -- "logs prune" is the command that truncates, and
this one never does. A missing file is reported, not created. Every open is
read-only, so a live log can be summarized while it is still being written.

Without --file the summary covers the project's own log, the same file
"logs path" prints: <data-dir>/logs/rush.log. With one or more --file flags
the summary covers exactly those files, in the order given, which is how a
captured stderr stream is read: "rush logs summary --file run.err".

Lines that are not JSON are counted as irregular rather than skipped
silently, because a log file that has stopped being JSON is itself the
anomaly an operator is looking for. A line that is JSON but has no level is
counted with an empty level and grouped under "(none)". An empty section
prints "(none)".

What the summary prints:
  * WARN and ERROR, grouped by msg, with the count, the level, and the
    fields of one representative line (the first one seen) so two messages
    with the same text can be told apart.
  * process start lines, in order: version, command, workspace, session and
    data directory of each process that logged one.
  * silence gaps: every stretch longer than --gap with no timestamped line
    at all, which is how a crashed run is told apart from an idle one.

Flags:
  --since <d>    only lines newer than a Go duration (30m, 24h), a day suffix
                 (7d), or a bare integer read as days
  --file <p>     log file to summarize; repeat for several. Omit to use the
                 project's own <data-dir>/logs/rush.log
  --gap <d>      how much quiet counts as a silence gap (default 15m)
  --json         one JSON object instead of text
  --data-dir <p> (inherited from the root command) the data directory whose
                 logs/rush.log is read when --file is not given
  --cwd <p>      (inherited) the working directory the project is resolved from`,
	Example: `
# Summarize the project's own log
rush logs summary

# A captured stderr stream, last 24 hours, as JSON
rush logs summary --file run.err --since 24h --json

# Several files at once, 30-minute silence threshold
rush logs summary --file a.log --file b.log --gap 30m
  `,
	Args: cobra.NoArgs,
	RunE: logsSummaryCmdRun,
}

func init() {
	logsSummaryCmd.Flags().String("since", "", "Only count lines newer than this: Go duration (30m, 24h), day suffix (7d), or a bare integer read as days")
	logsSummaryCmd.Flags().StringSlice("file", nil, "Log file to summarize (repeatable). Defaults to <data-dir>/logs/rush.log")
	logsSummaryCmd.Flags().Duration("gap", defaultSilenceGap, "How much time with no timestamped line counts as a silence gap")
	logsSummaryCmd.Flags().Bool("json", false, "Emit one JSON object instead of text")
}

// logEntry is one parsed log line. Fields holds every JSON field other than
// time/level/msg/source, rendered as "key=value" and sorted, so two lines
// that differ only in a field value are told apart reliably.
type logEntry struct {
	Time    time.Time
	HasTime bool
	Level   string
	Msg     string
	Fields  []string
}

// logMsgGroup is one (level, msg) pair with the number of times it was seen
// and the fields of its first occurrence.
type logMsgGroup struct {
	Level  string   `json:"level"`
	Msg    string   `json:"msg"`
	Count  int      `json:"count"`
	Fields []string `json:"fields"`
}

// logStart is one "process start" line: the facts that attribute the rest of
// that process's lines to a version, command and workspace.
type logStart struct {
	Time      time.Time `json:"time"`
	Command   string    `json:"command"`
	Version   string    `json:"version"`
	Workspace string    `json:"workspace"`
	Session   string    `json:"session"`
	DataDir   string    `json:"data_dir"`
}

// logSilence is one stretch with no timestamped line at all. The length is
// seconds, not a time.Duration: nanoseconds are not a number anyone reads or
// compares against a threshold.
type logSilence struct {
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	DurationSeconds float64   `json:"duration_seconds"`
}

// logSummary is the whole result of one scan.
type logSummary struct {
	Files     []string      `json:"files"`
	Lines     int           `json:"lines"`
	Irregular int           `json:"irregular"`
	Warnings  int           `json:"warnings"`
	Errors    int           `json:"errors"`
	MsgGroups []logMsgGroup `json:"msg_groups"`
	Starts    []logStart    `json:"starts"`
	Silences  []logSilence  `json:"silences"`
}

// logAccumulator carries the running state of one multi-file scan so the
// per-line logic does not take nine parameters.
type logAccumulator struct {
	summary logSummary
	groups  map[string]*logMsgGroup
	order   []string
	cutoff  time.Time
	gap     time.Duration
	// prev is the last timestamped line seen ACROSS files, so a gap spanning
	// two of them is still one gap and not two.
	havePrev bool
	prev     time.Time
}

// parseLogLine decodes one line. ok is false when the line is not JSON at
// all -- those are counted as irregular, never fatal. A JSON object without
// a "level" is a valid entry whose level is "", which the renderers group
// under "(none)".
func parseLogLine(line string) (logEntry, bool) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return logEntry{}, false
	}
	entry := logEntry{}
	if s, ok := raw["time"].(string); ok {
		// An unparsable timestamp is not fatal: the line still counts, it
		// just takes no part in the silence analysis.
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			entry.Time, entry.HasTime = t, true
		}
	}
	entry.Level, _ = raw["level"].(string)
	entry.Msg, _ = raw["msg"].(string)
	for k, v := range raw {
		switch k {
		case "time", "level", "msg", "source":
			// The call site is noise here: the level and msg carry the
			// event, and a nested source object does not read well as a
			// "key=value" field.
			continue
		}
		entry.Fields = append(entry.Fields, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(entry.Fields)
	return entry, true
}

// logFieldValue returns the value of one "key=value" field, or "" when the
// entry does not carry it.
func logFieldValue(fields []string, key string) string {
	prefix := key + "="
	for _, field := range fields {
		if strings.HasPrefix(field, prefix) {
			return strings.TrimPrefix(field, prefix)
		}
	}
	return ""
}

// addLine folds one log line into the running summary. It never fails: a line
// the parser cannot read is counted as irregular and dropped, which is the
// whole point -- one broken line must not abort a summary over thousands of
// good ones.
func (a *logAccumulator) addLine(line string) {
	a.summary.Lines++

	entry, ok := parseLogLine(line)
	if !ok {
		a.summary.Irregular++
		return
	}
	// --since drops whole lines by timestamp. An entry without a timestamp
	// is never dropped: there is nothing to compare it against.
	if entry.HasTime && !a.cutoff.IsZero() && entry.Time.Before(a.cutoff) {
		return
	}

	switch entry.Level {
	case "WARN":
		a.summary.Warnings++
	case "ERROR":
		a.summary.Errors++
	}
	if entry.Level == "WARN" || entry.Level == "ERROR" {
		a.addGroup(entry)
	}
	if entry.Msg == "process start" {
		a.addStart(entry)
	}
	a.addSilence(entry)
}

// addGroup counts one WARN or ERROR under its (level, msg) key, keeping the
// first occurrence's fields as the group's example.
func (a *logAccumulator) addGroup(entry logEntry) {
	key := entry.Level + "\x00" + entry.Msg
	group, seen := a.groups[key]
	if !seen {
		group = &logMsgGroup{
			Level:  entry.Level,
			Msg:    entry.Msg,
			Fields: append([]string(nil), entry.Fields...),
		}
		a.groups[key] = group
		a.order = append(a.order, key)
	}
	group.Count++
}

// addStart records one "process start" line.
func (a *logAccumulator) addStart(entry logEntry) {
	a.summary.Starts = append(a.summary.Starts, logStart{
		Time:      entry.Time,
		Command:   logFieldValue(entry.Fields, "command"),
		Version:   logFieldValue(entry.Fields, "version"),
		Workspace: logFieldValue(entry.Fields, "ws"),
		Session:   logFieldValue(entry.Fields, "session"),
		DataDir:   logFieldValue(entry.Fields, "data_dir"),
	})
}

// addSilence closes a silence gap when this timestamped line lands more than
// gap after the previous one. A gap of zero or less disables the analysis.
func (a *logAccumulator) addSilence(entry logEntry) {
	if !entry.HasTime || a.gap <= 0 {
		return
	}
	if a.havePrev && entry.Time.Sub(a.prev) > a.gap {
		a.summary.Silences = append(a.summary.Silences, logSilence{
			From:            a.prev,
			To:              entry.Time,
			DurationSeconds: entry.Time.Sub(a.prev).Seconds(),
		})
	}
	a.prev, a.havePrev = entry.Time, true
}

// addFile reads one log file into the accumulator, opening it for reading and
// nothing else. A file that cannot be opened or read is an error: the
// operator named it, and silently skipping it would under-report.
func (a *logAccumulator) addFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only: nothing to flush

	// ReadString, not Scanner: a line longer than Scanner's default token
	// size is legal in a log, and the last line may lack its newline.
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			a.addLine(line)
		}
		if err != nil {
			// ReadString reports io.EOF for a file that simply ended, with
			// or without a final newline, which is the normal case here.
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
}

// finish orders the message groups and returns the summary. Groups are
// ordered by count (descending), then by level and msg so the order is stable
// when two of them tie.
func (a *logAccumulator) finish() logSummary {
	sorted := make([]*logMsgGroup, 0, len(a.order))
	for _, key := range a.order {
		sorted = append(sorted, a.groups[key])
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Count != sorted[j].Count {
			return sorted[i].Count > sorted[j].Count
		}
		if sorted[i].Level != sorted[j].Level {
			return sorted[i].Level < sorted[j].Level
		}
		return sorted[i].Msg < sorted[j].Msg
	})
	a.summary.MsgGroups = make([]logMsgGroup, 0, len(sorted))
	for _, group := range sorted {
		a.summary.MsgGroups = append(a.summary.MsgGroups, *group)
	}
	// An empty section is [] and not null in --json, so a consumer reading
	// the sections does not have to special-case "nothing was found".
	if a.summary.Starts == nil {
		a.summary.Starts = []logStart{}
	}
	if a.summary.Silences == nil {
		a.summary.Silences = []logSilence{}
	}
	return a.summary
}

// summarizeFiles reads each named file in order and builds the summary over
// all of them.
func summarizeFiles(files []string, since, gap time.Duration) (logSummary, error) {
	acc := &logAccumulator{
		summary: logSummary{Files: append([]string(nil), files...)},
		groups:  make(map[string]*logMsgGroup),
		gap:     gap,
	}
	if since > 0 {
		acc.cutoff = time.Now().Add(-since)
	}
	for _, path := range files {
		if err := acc.addFile(path); err != nil {
			return logSummary{}, err
		}
	}
	return acc.finish(), nil
}

func logsSummaryCmdRun(cmd *cobra.Command, args []string) error {
	files, _ := cmd.Flags().GetStringSlice("file")
	sinceStr, _ := cmd.Flags().GetString("since")
	gap, _ := cmd.Flags().GetDuration("gap")
	asJSON, _ := cmd.Flags().GetBool("json")

	var since time.Duration
	if sinceStr != "" {
		parsed, err := parseSinceDuration(sinceStr)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		since = parsed
	}

	if len(files) == 0 {
		dataDirFlag, _ := cmd.Flags().GetString("data-dir")
		cwd, err := ResolveCwd(cmd)
		if err != nil {
			return err
		}
		// The same lightweight read-only resolution "sessions audit" uses:
		// --data-dir first, then the project's configured data_directory,
		// then <cwd>/.rush. Never config.Load's write path.
		dataDir, err := config.ResolveDataDirectory(cwd, dataDirFlag)
		if err != nil {
			return fmt.Errorf("failed to resolve data directory: %w", err)
		}
		p := filepath.Join(dataDir, "logs", "rush.log")
		if _, err := os.Stat(p); err != nil {
			// A missing default log is not an error: nothing has logged
			// yet in this project, and there is nothing to create either.
			fmt.Fprintln(os.Stderr, "(no log file)")
			return nil
		}
		files = []string{p}
	}

	summary, err := summarizeFiles(files, since, gap)
	if err != nil {
		return err
	}
	if asJSON {
		return renderLogSummaryJSON(os.Stdout, summary)
	}
	return renderLogSummary(os.Stdout, summary)
}

// renderLogSummaryJSON writes one JSON object for a script to read.
func renderLogSummaryJSON(w io.Writer, s logSummary) error {
	if err := json.NewEncoder(w).Encode(s); err != nil {
		return fmt.Errorf("encode log summary: %w", err)
	}
	return nil
}

// renderLogSummary writes the text an operator reads: the files, the line
// counters, then one block per question the summary answers. A block with
// nothing in it prints "(none)" so an empty section is never mistaken for a
// section that failed to run.
func renderLogSummary(w io.Writer, s logSummary) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintln(tw, "FILES")
	for _, path := range s.Files {
		fmt.Fprintf(tw, "  %s\n", path)
	}
	fmt.Fprintf(tw, "LINES: %d   IRREGULAR: %d   WARN: %d   ERROR: %d\n\n",
		s.Lines, s.Irregular, s.Warnings, s.Errors)

	fmt.Fprintln(tw, "BY MESSAGE")
	if len(s.MsgGroups) == 0 {
		fmt.Fprintln(tw, "  (none)")
	}
	for _, group := range s.MsgGroups {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", group.Level, group.Count, group.Msg)
		if len(group.Fields) > 0 {
			fmt.Fprintf(tw, "  fields: %s\n", strings.Join(group.Fields, ", "))
		}
	}
	fmt.Fprintln(tw)

	fmt.Fprintln(tw, "PROCESS STARTS")
	if len(s.Starts) == 0 {
		fmt.Fprintln(tw, "  (none)")
	}
	for _, start := range s.Starts {
		if start.DataDir != "" {
			fmt.Fprintf(tw, "  %s\t%s\t%s\tws=%s\tsession=%s\tdata_dir=%s\n",
				start.Time.Format(time.RFC3339), start.Version, start.Command,
				start.Workspace, start.Session, start.DataDir)
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\tws=%s\tsession=%s\n",
			start.Time.Format(time.RFC3339), start.Version, start.Command,
			start.Workspace, start.Session)
	}
	fmt.Fprintln(tw)

	fmt.Fprintln(tw, "SILENCE GAPS")
	if len(s.Silences) == 0 {
		fmt.Fprintln(tw, "  (none)")
	}
	for _, silence := range s.Silences {
		fmt.Fprintf(tw, "  %s -> %s\t(%s)\n",
			silence.From.Format(time.RFC3339),
			silence.To.Format(time.RFC3339),
			time.Duration(silence.DurationSeconds)*time.Second)
	}
	return tw.Flush()
}
