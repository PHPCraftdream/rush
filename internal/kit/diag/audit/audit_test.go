package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func setDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	SetDirFunc(func() string { return dir })
	t.Cleanup(func() { SetDirFunc(nil) })
	return dir
}

func readRecords(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(string(b), "\n")
	require.Equal(t, "", lines[len(lines)-1])
	lines = lines[:len(lines)-1]
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		records = append(records, rec)
	}
	return records
}

func TestWriteRecordFields(t *testing.T) {
	dir := setDir(t)
	require.NoError(t, Write(Event{"kind": "command", "cmd": "rush run", "exit": 0}))
	paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	data, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(string(data), "\n"))
	records := readRecords(t, paths[0])
	require.Len(t, records, 1)
	rec := records[0]
	require.Equal(t, "command", rec["kind"])
	require.Equal(t, "rush run", rec["cmd"])
	require.Equal(t, float64(0), rec["exit"])
	_, err = time.Parse(time.RFC3339Nano, rec["ts"].(string))
	require.NoError(t, err)
	require.Equal(t, float64(os.Getpid()), rec["pid"])
	require.Equal(t, float64(os.Getppid()), rec["ppid"])
	_, ok := rec["parent"].(string)
	require.True(t, ok)
	require.Equal(t, LaunchCwd(), rec["launch_cwd"])
	require.NotEmpty(t, LaunchCwd())
	_, ok = rec["truncated"]
	require.False(t, ok)
}

func TestLaunchCwdSurvivesChdir(t *testing.T) {
	start := LaunchCwd()
	require.NotEmpty(t, start)
	t.Chdir(t.TempDir())
	current, err := os.Getwd()
	require.NoError(t, err)
	require.Equal(t, start, LaunchCwd())
	require.NotEqual(t, start, current)
}

func TestWriteTruncation(t *testing.T) {
	t.Run("long string", func(t *testing.T) {
		dir := setDir(t)
		require.NoError(t, Write(Event{"big": strings.Repeat("x", 400)}))
		paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
		require.NoError(t, err)
		require.Len(t, paths, 1)
		rec := readRecords(t, paths[0])[0]
		big := rec["big"].(string)
		require.Equal(t, 300, len([]rune(big)))
		require.True(t, strings.HasSuffix(big, "…"))
	})
	t.Run("oversized record", func(t *testing.T) {
		dir := setDir(t)
		ev := make(Event, 30)
		for i := range 30 {
			ev[fmt.Sprintf("f%02d", i)] = strings.Repeat("y", 300)
		}
		require.NoError(t, Write(ev))
		paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
		require.NoError(t, err)
		require.Len(t, paths, 1)
		data, err := os.ReadFile(paths[0])
		require.NoError(t, err)
		line := strings.TrimSuffix(string(data), "\n")
		require.LessOrEqual(t, len(line), maxRecordBytes)
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		require.Equal(t, true, rec["truncated"])
		count := 0
		for key := range rec {
			if strings.HasPrefix(key, "f") {
				count++
			}
		}
		require.Less(t, count, 30)
	})
}

func TestWriteConcurrentAppend(t *testing.T) {
	dir := setDir(t)
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for w := range 50 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 20 {
				if err := Write(Event{"i": i, "worker": w}); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
	records := readRecords(t, paths[0])
	require.Len(t, records, 1000)
	for _, rec := range records {
		_, ok := rec["pid"].(float64)
		require.True(t, ok)
	}
}

func TestPruneRemovesOldFiles(t *testing.T) {
	dir := setDir(t)
	today := now().Format(dateLayout)
	files := []string{"audit-2020-01-01.jsonl", "audit-" + today + ".jsonl", "other.txt", "audit-notadate.jsonl"}
	for _, name := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600))
	}
	removed, err := Prune(30 * 24 * time.Hour)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	_, statErr := os.Stat(filepath.Join(dir, files[0]))
	require.True(t, os.IsNotExist(statErr), "the old audit file must be deleted")
	for _, name := range files[1:] {
		_, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err)
	}
	paths, err := Files()
	require.NoError(t, err)
	expected := []string{filepath.Join(dir, "audit-"+today+".jsonl"), filepath.Join(dir, "audit-notadate.jsonl")}
	sort.Strings(expected)
	require.Equal(t, expected, paths)
}

func TestWriteWithoutDirFunc(t *testing.T) {
	t.Cleanup(func() { SetDirFunc(nil) })
	SetDirFunc(nil)
	require.Error(t, Write(Event{"k": "v"}))
	SetDirFunc(func() string { return "" })
	require.Error(t, Write(Event{"k": "v"}))
	SetDirFunc(nil)
	paths, err := Files()
	require.NoError(t, err)
	require.Nil(t, paths)
	_, err = Prune(time.Hour)
	require.Error(t, err)
}

func TestFileDateRollsWithClock(t *testing.T) {
	dir := setDir(t)
	realNow := now
	t.Cleanup(func() { now = realNow })
	day1 := time.Date(2026, 3, 10, 23, 59, 0, 0, time.Local)
	now = func() time.Time { return day1 }
	require.NoError(t, Write(Event{"n": 1}))
	now = func() time.Time { return day1.Add(2 * time.Hour) }
	require.NoError(t, Write(Event{"n": 2}))
	paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	require.NoError(t, err)
	sort.Strings(paths)
	require.Equal(t, []string{filepath.Join(dir, "audit-2026-03-10.jsonl"), filepath.Join(dir, "audit-2026-03-11.jsonl")}, paths)
	for i, value := range []float64{1, 2} {
		records := readRecords(t, paths[i])
		require.Len(t, records, 1)
		require.Equal(t, value, records[0]["n"])
	}
}

type evilMarshal struct{}

func (evilMarshal) MarshalJSON() ([]byte, error) { panic("json boom") }

func TestPruneZeroDeletesAll(t *testing.T) {
	dir := setDir(t)
	today := now().Format(dateLayout)
	files := []string{
		"audit-2020-01-01.jsonl",
		"audit-" + today + ".jsonl",
		"audit-2999-01-01.jsonl",
		"other.txt",
	}
	for _, name := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600))
	}
	removed, err := Prune(0)
	require.NoError(t, err)
	require.Equal(t, 3, removed)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "other.txt", entries[0].Name())
}

func TestWriteSizeBoundTable(t *testing.T) {
	type note struct{ Body string }
	long := strings.Repeat("z", 10000)
	nested := map[string]any{"l1": map[string]any{"l2": map[string]any{"l3": long}}}
	deep := map[string]any{}
	cur := deep
	for range maxDepth + 4 {
		next := map[string]any{}
		cur["down"] = next
		cur = next
	}
	cur["bottom"] = long
	many := make(map[string]any, 500)
	for i := range 500 {
		many[fmt.Sprintf("k%03d", i)] = strings.Repeat("m", 30)
	}
	tests := []struct {
		name string
		ev   Event
	}{
		{"int slice", Event{"nums": func() []int {
			s := make([]int, 2000)
			for i := range s {
				s[i] = i
			}
			return s
		}()}},
		{"nested string", Event{"nested": nested}},
		{"deeper than cap", Event{"deep": deep}},
		{"struct value", Event{"note": note{Body: long}}},
		{"many keys", Event(many)},
		{"huge key", Event{strings.Repeat("K", 4000): "v"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := setDir(t)
			require.NoError(t, Write(tc.ev))
			paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
			require.NoError(t, err)
			require.Len(t, paths, 1)
			data, err := os.ReadFile(paths[0])
			require.NoError(t, err)
			line := strings.TrimSuffix(string(data), "\n")
			require.LessOrEqual(t, len(line), 3500, "the record must fit the byte cap")
			var rec map[string]any
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			require.Equal(t, float64(os.Getpid()), rec["pid"])
			require.Equal(t, LaunchCwd(), rec["launch_cwd"])
			_, ok := rec["ts"].(string)
			require.True(t, ok)
		})
	}
}

func TestWriteDoesNotMutateCaller(t *testing.T) {
	_ = setDir(t)
	ev := Event{
		"nested": map[string]any{"deep": []string{strings.Repeat("a", 400)}},
		"list":   []string{strings.Repeat("b", 400)},
	}
	before, err := json.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, Write(ev))
	after, err := json.Marshal(ev)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "Write must not mutate the caller's event")
}

func TestWriteRecoversFromPanics(t *testing.T) {
	t.Run("panicking resolver", func(t *testing.T) {
		t.Cleanup(func() { SetDirFunc(nil) })
		SetDirFunc(func() string { panic("dir boom") })
		require.Error(t, Write(Event{"k": "v"}))
		_, perr := Prune(time.Hour)
		require.Error(t, perr)
	})
	t.Run("panicking marshaler", func(t *testing.T) {
		dir := setDir(t)
		require.Error(t, Write(Event{"v": evilMarshal{}}))
		paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
		require.NoError(t, err)
		require.Empty(t, paths, "no file may be created when the record cannot be built")
	})
}

func TestPruneAndFilesMissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	SetDirFunc(func() string { return missing })
	t.Cleanup(func() { SetDirFunc(nil) })
	removed, err := Prune(time.Hour)
	require.NoError(t, err)
	require.Zero(t, removed)
	paths, err := Files()
	require.NoError(t, err)
	require.Nil(t, paths)
}

func TestWriteCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "central", "nested")
	SetDirFunc(func() string { return dir })
	t.Cleanup(func() { SetDirFunc(nil) })
	require.NoError(t, Write(Event{"k": "v"}))
	paths, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	require.NoError(t, err)
	require.Len(t, paths, 1)
}
