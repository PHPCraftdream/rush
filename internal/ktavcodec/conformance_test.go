package ktavcodec

// Spec-conformance runner for the vendored Ktav 0.8.1 corpus
// (testdata/spec-0.8.1). It mirrors upstream's ref-conformance_test.go
// and ref-corpus_manifest_test.go for the pure backend, using
// github.com/ktav-lang/golang/pure directly: every category is
// executed, every discovered fixture runs exactly once, and the
// manifest's exact per-category counts are enforced. Unknown
// categories, zero-case categories, skipped cases or missing
// companions fail the run (design § 8, "Spec conformance gate").

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/ktav-lang/golang/pure"
	"github.com/stretchr/testify/require"
)

// corpusRoot is the vendored corpus, relative to this package.
var corpusRoot = filepath.Join("testdata", "spec-0.8.1")

// expectedCounts pins the manifest counts the runner implements; a
// manifest with a different shape or count must fail rather than be
// guessed at.
var expectedCounts = map[string]int{
	"valid":                     228,
	"invalid":                   90,
	"unrepresentable":           5,
	"parseable-unrepresentable": 4,
	"strict-lossy":              13,
}

// knownCategories is the closed set of category directory names.
var knownCategories = map[string]bool{
	"valid": true, "invalid": true, "unrepresentable": true,
	"parseable-unrepresentable": true, "strict-lossy": true,
}

// corpusManifest mirrors manifest.json (schema_version 1).
type corpusManifest struct {
	Comment       string                 `json:"$comment"`
	SchemaVersion int                    `json:"schema_version"`
	Categories    map[string]corpusCount `json:"categories"`
	FixtureFlags  []corpusFixtureFlag    `json:"fixture_flags"`
}

type corpusCount struct {
	Count int `json:"count"`
}

type corpusFixtureFlag struct {
	Category string   `json:"category"`
	Fixture  string   `json:"fixture"`
	Flags    []string `json:"flags"`
	Note     string   `json:"note"`
}

// loadCorpusManifest reads and shape-checks manifest.json.
func loadCorpusManifest(t *testing.T) *corpusManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusRoot, "manifest.json"))
	require.NoError(t, err)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m corpusManifest
	require.NoError(t, dec.Decode(&m), "decode manifest.json")
	require.ErrorIs(t, dec.Decode(new(any)), io.EOF,
		"manifest.json has trailing data")
	require.Equal(t, 1, m.SchemaVersion, "unsupported schema_version")
	require.NotEmpty(t, m.Categories, "manifest has no categories")
	return m
}

// fixturePart splits a category-relative file into stem and companion
// suffix ("ktav", "json", "canonical.ktav").
func fixturePart(rel string) (stem, suffix string, ok bool) {
	for _, ext := range []string{".canonical.ktav", ".ktav", ".json"} {
		if strings.HasSuffix(rel, ext) {
			return strings.TrimSuffix(rel, ext),
				strings.TrimPrefix(ext, "."), true
		}
	}
	return "", "", false
}

// companionSet is the required companion file set per category.
func companionSet(category string) map[string]bool {
	switch category {
	case "valid":
		return map[string]bool{
			"ktav": true, "json": true,
			"canonical.ktav": true,
		}
	case "invalid", "parseable-unrepresentable", "strict-lossy":
		return map[string]bool{"ktav": true, "json": true}
	default:
		return map[string]bool{"json": true}
	}
}

// discoverFixtures validates one category directory's shape and
// returns its stems. Enforces: known category, non-empty, exact
// manifest count, complete companion sets, no unexpected files.
func discoverFixtures(t *testing.T, m *corpusManifest, category string,
	seen map[string]bool,
) map[string]bool {
	t.Helper()
	count, declared := m.Categories[category]
	require.True(t, declared, "manifest missing category %q", category)
	require.Greater(t, count.Count, 0,
		"category %q is empty; empty categories fail the gate", category)

	stems := map[string]bool{}
	root := filepath.Join(corpusRoot, category)
	require.NoError(t, filepath.WalkDir(root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			require.NoError(t, walkErr)
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			require.NoError(t, err)
			stem, suffix, ok := fixturePart(filepath.ToSlash(rel))
			require.True(t, ok, "unexpected file in %s: %q",
				category, filepath.ToSlash(rel))
			require.NotContains(t, stems, stem+":"+suffix,
				"duplicate companion for %s/%s", category, stem)
			stems[stem] = true
			return nil
		}))
	require.Len(t, stems, count.Count,
		"category %q fixture count mismatch", category)
	want := companionSet(category)
	for stem := range stems {
		for suffix := range want {
			_, err := os.Stat(filepath.Join(root,
				filepath.FromSlash(stem)+"."+suffix))
			require.NoError(t, err,
				"fixture %s/%s missing .%s companion",
				category, stem, suffix)
		}
	}
	seen[category] = true
	return stems
}

// TestCorpusShape pins the vendored corpus to its manifest before any
// fixture runs: unknown directories, unexpected files, wrong counts
// or missing companions fail here.
func TestCorpusShape(t *testing.T) {
	m := loadCorpusManifest(t)
	require.Equal(t, expectedCounts, categoryCounts(m),
		"manifest counts drifted from the runner's pinned expectations")

	entries, err := os.ReadDir(corpusRoot)
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if name == "manifest.json" || name == "boundary-fixtures.json" ||
			strings.HasPrefix(name, "LICENSE-") {
			continue
		}
		require.True(t, entry.IsDir(), "unexpected corpus file %q", name)
		require.Contains(t, knownCategories, name,
			"unknown corpus category directory %q", name)
	}
	for category := range expectedCounts {
		discoverFixtures(t, m, category, seen)
	}
	require.Len(t, seen, len(expectedCounts),
		"a declared category directory is missing")
}

// categoryCounts extracts the declared count map.
func categoryCounts(m *corpusManifest) map[string]int {
	out := make(map[string]int, len(m.Categories))
	for k, v := range m.Categories {
		out[k] = v.Count
	}
	return out
}

// rawBytesFixtures returns the manifest's raw_bytes fixture set.
func rawBytesFixtures(m *corpusManifest) map[string]bool {
	out := map[string]bool{}
	for _, f := range m.FixtureFlags {
		for _, flag := range f.Flags {
			if flag == "raw_bytes" {
				out[f.Category+"/"+f.Fixture] = true
			}
		}
	}
	return out
}

// readFixture reads a fixture's bytes, refusing to decode flagged
// raw_bytes fixtures as text (they are deliberately invalid UTF-8).
func readFixture(t *testing.T, category, stem, suffix string,
	rawBytes map[string]bool,
) []byte {
	t.Helper()
	path := filepath.Join(corpusRoot, category,
		filepath.FromSlash(stem)+"."+suffix)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	if suffix == "ktav" && !rawBytes[category+"/"+stem] {
		require.True(t, utf8.Valid(raw),
			"unflagged fixture %s/%s is not valid UTF-8",
			category, stem)
	}
	return raw
}

// ─── oracle decoding (port of ref-pureconsumer-oracle.go) ─────────────

// decodeOracle parses a reference JSON oracle and lifts numbers into
// the shapes pure.Loads produces (int64, *big.Int, float64).
func decodeOracle(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v), "oracle JSON decode")
	require.ErrorIs(t, dec.Decode(new(any)), io.EOF,
		"oracle JSON has trailing data")
	return liftOracle(t, v)
}

func liftOracle(tb testing.TB, v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			x[k] = liftOracle(tb, child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = liftOracle(tb, child)
		}
		return x
	case json.Number:
		s := string(x)
		if !strings.ContainsAny(s, ".eE") {
			if n, ok := new(big.Int).SetString(s, 10); ok {
				if n.IsInt64() {
					return n.Int64()
				}
				return n
			}
		}
		f, err := x.Float64()
		require.NoError(tb, err, "bad oracle number %q", s)
		return f
	default:
		return v
	}
}

// structEqual compares a pure.Loads result with the lifted oracle.
func structEqual(a, b any) bool {
	switch av := a.(type) {
	case *big.Int:
		bv, ok := b.(*big.Int)
		return ok && av.Cmp(bv) == 0
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case map[string]any:
		bm, ok := b.(map[string]any)
		if !ok || len(av) != len(bm) {
			return false
		}
		for k, v := range av {
			bv, ok := bm[k]
			if !ok || !structEqual(v, bv) {
				return false
			}
		}
		return true
	case []any:
		bs, ok := b.([]any)
		if !ok || len(av) != len(bs) {
			return false
		}
		for i := range av {
			if !structEqual(av[i], bs[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// decodeUnrepOracle reads an unrepresentable / parseable-
// unrepresentable oracle: (value, reason), lifting the {"$float":...}
// marker to a non-finite float64.
func decodeUnrepOracle(t *testing.T, raw []byte) (any, string) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc struct {
		Value                 any    `json:"value"`
		UnrepresentableReason string `json:"unrepresentable_reason"`
	}
	require.NoError(t, dec.Decode(&doc), "unrep oracle decode")
	require.ErrorIs(t, dec.Decode(new(any)), io.EOF,
		"unrep oracle has trailing data")
	require.NotEmpty(t, doc.UnrepresentableReason,
		"unrep oracle has no reason")
	return liftMarker(t, doc.Value), doc.UnrepresentableReason
}

func liftMarker(tb testing.TB, v any) any {
	if m, ok := v.(map[string]any); ok && len(m) == 1 {
		if s, ok := m["$float"].(string); ok {
			switch s {
			case "NaN":
				return math.NaN()
			case "Infinity":
				return math.Inf(1)
			case "-Infinity":
				return math.Inf(-1)
			}
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			x[k] = liftMarker(tb, child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = liftMarker(tb, child)
		}
		return x
	case json.Number:
		return liftOracle(tb, x)
	default:
		return v
	}
}

// assertInvalidOracle compares a parse error against the invalid
// fixture's full error-envelope oracle (optional fields checked only
// when present; expected_* takes precedence).
func assertInvalidOracle(t *testing.T, err error, raw []byte) {
	t.Helper()
	var oracle struct {
		ExpectedError     string     `json:"expected_error"`
		ExpectedReason    *string    `json:"expected_reason"`
		Reason            *string    `json:"reason"`
		ExpectedBody      *string    `json:"expected_body"`
		Body              *string    `json:"body"`
		ExpectedCanonical *string    `json:"expected_canonical"`
		Canonical         *string    `json:"canonical"`
		ExpectedSection   *string    `json:"expected_spec_section"`
		SpecSection       *string    `json:"spec_section"`
		ExpectedLine      *int       `json:"expected_line"`
		Line              *int       `json:"line"`
		ExpectedLineText  *string    `json:"expected_line_text"`
		LineText          *string    `json:"line_text"`
		ExpectedPath      *[]string  `json:"expected_path"`
		Path              *[]string  `json:"path"`
		ExpectedSpan      *pure.Span `json:"expected_span"`
		Span              *pure.Span `json:"span"`
	}
	require.NoError(t, json.Unmarshal(raw, &oracle),
		"decode invalid-fixture oracle")
	require.NotEmpty(t, oracle.ExpectedError,
		"invalid-fixture oracle has no expected_error")
	var got *pure.Error
	require.ErrorAs(t, err, &got,
		"parse error is not *pure.Error: %v", err)
	require.Equal(t, oracle.ExpectedError, got.Class, "error class")
	firstString := func(a, b *string) *string {
		if a != nil {
			return a
		}
		return b
	}
	firstInt := func(a, b *int) *int {
		if a != nil {
			return a
		}
		return b
	}
	if want := firstString(oracle.ExpectedReason, oracle.Reason); want != nil {
		require.Equal(t, *want, got.Reason, "error reason")
	}
	if want := firstString(oracle.ExpectedBody, oracle.Body); want != nil {
		require.Equal(t, *want, got.Body, "error body")
	}
	if want := firstString(oracle.ExpectedCanonical, oracle.Canonical); want != nil {
		require.Equal(t, *want, got.Canonical, "error canonical")
	}
	if want := firstString(oracle.ExpectedSection, oracle.SpecSection); want != nil {
		require.Equal(t, *want, got.SpecSection, "error spec_section")
	}
	if want := firstInt(oracle.ExpectedLine, oracle.Line); want != nil {
		require.Equal(t, *want, got.Line, "error line")
	}
	if want := firstString(oracle.ExpectedLineText, oracle.LineText); want != nil {
		require.Equal(t, *want, got.LineText, "error line_text")
	}
	if want := firstPath(oracle.ExpectedPath, oracle.Path); want != nil {
		require.Equal(t, *want, got.Path, "error path")
	}
	if want := firstSpan(oracle.ExpectedSpan, oracle.Span); want != nil {
		require.Equal(t, *want, got.Span, "error span")
	}
}

func firstPath(a, b *[]string) *[]string {
	if a != nil {
		return a
	}
	return b
}

func firstSpan(a, b *pure.Span) *pure.Span {
	if a != nil {
		return a
	}
	return b
}

// assertUnrepresentable pins the writer-refusal envelope to the
// corpus reason (class, reason and implied spec section).
func assertUnrepresentable(t *testing.T, err error, reason, label string) {
	t.Helper()
	section, ok := unrepresentableSpecSection(reason)
	require.True(t, ok, "unknown unrepresentable reason %q", reason)
	var got *pure.Error
	require.ErrorAs(t, err, &got, "%s: not *pure.Error: %v", label, err)
	require.Equal(t, "UnrepresentableAt", got.Class, label)
	require.Equal(t, reason, got.Reason, label)
	require.Equal(t, section, got.SpecSection, label)
}

func unrepresentableSpecSection(reason string) (string, bool) {
	switch reason {
	case "CRByte", "BothFormsRequired", "LeadingWhitespaceCollision",
		"TrailingWhitespaceCollision":
		return "§5.9.7", true
	case "EmptyKeyName", "NonFiniteFloat", "ScalarRoot":
		return "§5.9.0", true
	default:
		return "", false
	}
}

// ─── fixture execution recorder ────────────────────────────────────────

// fixtureRecorder records which fixtures the category runners
// actually executed. It is package-level state owned by the one
// parent test below and guarded by a mutex because the runners are
// parallel.
type fixtureRecorder struct {
	mu   sync.Mutex
	seen map[string]int
}

func newFixtureRecorder() *fixtureRecorder {
	return &fixtureRecorder{seen: map[string]int{}}
}

func (r *fixtureRecorder) record(category, stem string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen[category+"/"+stem]++
}

func (r *fixtureRecorder) count(category, stem string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[category+"/"+stem]
}

func (r *fixtureRecorder) categoryCount(category string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for key, c := range r.seen {
		if strings.HasPrefix(key, category+"/") {
			n += c
		}
	}
	return n
}

// runConformanceFixture dispatches one fixture to its category
// runner; the semantic assertions live in the per-category helpers.
func runConformanceFixture(t *testing.T, category, stem string,
	rawBytes map[string]bool,
) {
	t.Helper()
	switch category {
	case "valid":
		runValidFixture(t, stem, rawBytes)
	case "invalid":
		runInvalidFixture(t, stem, rawBytes)
	case "unrepresentable":
		runUnrepresentableFixture(t, stem)
	case "parseable-unrepresentable":
		runParseableUnrepresentableFixture(t, stem, rawBytes)
	case "strict-lossy":
		runStrictLossyFixture(t, stem, rawBytes)
	default:
		t.Fatalf("unknown corpus category %q", category)
	}
}

// runValidFixture: pure.Loads must parse each .ktav into the oracle
// value, and CanonicalFromSource must reproduce the .canonical.ktav
// oracle byte-for-byte.
func runValidFixture(t *testing.T, stem string, rawBytes map[string]bool) {
	t.Helper()
	src := readFixture(t, "valid", stem, "ktav", rawBytes)
	oracle := readFixture(t, "valid", stem, "json", rawBytes)
	canonical := readFixture(t, "valid", stem, "canonical.ktav", rawBytes)

	got, err := pure.Loads(string(src))
	require.NoError(t, err, "--- input ---\n%s", src)
	require.True(t, structEqual(got, decodeOracle(t, oracle)),
		"value mismatch\n--- input ---\n%s", src)

	// Canonical text must match the oracle byte-for-byte. This is the
	// upstream oracle for the writer, not a round-trip self-check.
	out, err := pure.CanonicalFromSource(string(src))
	require.NoError(t, err, "--- input ---\n%s", src)
	require.Equal(t, string(canonical), out,
		"canonical mismatch\n--- input ---\n%s", src)
}

// runInvalidFixture: pure.Loads must refuse and the error envelope
// must match the oracle exactly.
func runInvalidFixture(t *testing.T, stem string, rawBytes map[string]bool) {
	t.Helper()
	src := readFixture(t, "invalid", stem, "ktav", rawBytes)
	oracle := readFixture(t, "invalid", stem, "json", rawBytes)
	_, err := pure.Loads(string(src))
	require.Error(t, err, "expected parse error\n---\n%s", src)
	assertInvalidOracle(t, err, oracle)
}

// runUnrepresentableFixture: pure.Dumps and pure.EmitCanonical must
// refuse each oracle value with the pinned UnrepresentableAt
// envelope.
func runUnrepresentableFixture(t *testing.T, stem string) {
	t.Helper()
	oracle := readFixture(t, "unrepresentable", stem, "json", nil)
	value, reason := decodeUnrepOracle(t, oracle)
	_, err1 := pure.Dumps(value)
	_, err2 := pure.EmitCanonical(value)
	assertUnrepresentable(t, err1, reason, "Dumps")
	assertUnrepresentable(t, err2, reason, "EmitCanonical")
}

// runParseableUnrepresentableFixture: pure.Loads must accept and
// match the oracle value, while CanonicalFromSource, Dumps and
// EmitCanonical must all refuse with the pinned reason.
func runParseableUnrepresentableFixture(t *testing.T, stem string,
	rawBytes map[string]bool,
) {
	t.Helper()
	src := readFixture(t, "parseable-unrepresentable", stem, "ktav",
		rawBytes)
	oracle := readFixture(t, "parseable-unrepresentable", stem, "json",
		rawBytes)
	want, reason := decodeUnrepOracle(t, oracle)

	got, err := pure.Loads(string(src))
	require.NoError(t, err, "--- input ---\n%s", src)
	require.True(t, structEqual(got, want),
		"value mismatch\n--- input ---\n%s", src)

	_, err = pure.CanonicalFromSource(string(src))
	assertUnrepresentable(t, err, reason, "CanonicalFromSource")
	_, err = pure.Dumps(got)
	assertUnrepresentable(t, err, reason, "Dumps")
	_, err = pure.EmitCanonical(got)
	assertUnrepresentable(t, err, reason, "EmitCanonical")
}

// runStrictLossyFixture: pure.Loads must accept and match the lax
// oracle value while pure.LoadsStrict refuses with the oracle's
// exact class, body and canonical fields.
func runStrictLossyFixture(t *testing.T, stem string,
	rawBytes map[string]bool,
) {
	t.Helper()
	src := readFixture(t, "strict-lossy", stem, "ktav", rawBytes)
	oracle := readFixture(t, "strict-lossy", stem, "json", rawBytes)
	var doc struct {
		LaxValue      json.RawMessage `json:"lax_value"`
		ExpectedError string          `json:"expected_error"`
		Body          string          `json:"body"`
		Canonical     string          `json:"canonical"`
	}
	require.NoError(t, json.Unmarshal(oracle, &doc),
		"decode strict-lossy oracle")
	want := decodeOracle(t, doc.LaxValue)

	got, err := pure.Loads(string(src))
	require.NoError(t, err, "--- input ---\n%s", src)
	require.True(t, structEqual(got, want),
		"lax value mismatch\n--- input ---\n%s", src)

	_, err = pure.LoadsStrict(string(src))
	require.Error(t, err, "strict parse must refuse\n---\n%s", src)
	var pe *pure.Error
	require.ErrorAs(t, err, &pe,
		"want *pure.Error, got %T (%v)", err, err)
	require.Equal(t, doc.ExpectedError, pe.Class, "class")
	require.Equal(t, doc.Body, pe.Body, "body")
	require.Equal(t, doc.Canonical, pe.Canonical, "canonical")
}

// TestConformanceCorpus is the single parent test owning the fixture
// recorder: the five category runners above run as its subtests, and
// the parent's cleanup asserts that every fixture discovered on disk
// was executed exactly once in its category, that the executed count
// matches the manifest, and that no unknown category directory
// exists. A runner that skips a fixture or early-returns before
// recording therefore fails here rather than silently narrowing the
// gate.
func TestConformanceCorpus(t *testing.T) {
	m := loadCorpusManifest(t)
	rawBytes := rawBytesFixtures(m)
	rec := newFixtureRecorder()
	for category := range expectedCounts {
		category := category
		discovered := discoverFixtures(t, m, category, map[string]bool{})
		t.Run(category, func(t *testing.T) {
			for stem := range discovered {
				stem := stem
				t.Run(stem, func(t *testing.T) {
					t.Parallel()
					rec.record(category, stem)
					runConformanceFixture(t, category, stem, rawBytes)
				})
			}
		})
	}
	t.Cleanup(func() {
		for category := range expectedCounts {
			discovered := discoverFixtures(t, m, category,
				map[string]bool{})
			for stem := range discovered {
				require.Equal(t, 1, rec.count(category, stem),
					"fixture %s/%s executed %d times, want exactly 1",
					category, stem, rec.count(category, stem))
			}
			require.Equal(t, m.Categories[category].Count,
				rec.categoryCount(category),
				"category %q executed-fixture count mismatch", category)
		}
		entries, err := os.ReadDir(corpusRoot)
		require.NoError(t, err)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			require.Contains(t, knownCategories, entry.Name(),
				"unknown corpus category directory %q", entry.Name())
		}
	})
}
