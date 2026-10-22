package ktavcodec

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// num is a shorthand for building json.Number trees in tables.
func num(s string) json.Number { return json.Number(s) }

// obj builds a map from key/value pairs.
func obj(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestParseJSONLosslessSingleDocument(t *testing.T) {
	t.Parallel()
	// Mutation caught: a decoder that accepts trailing data or streams
	// more than one document would silently merge unrelated config
	// files; the first assertion pins the happy path.
	t1, err := ParseJSONLossless([]byte(`{"a":1}`))
	require.NoError(t, err)
	require.Equal(t, obj("a", num("1")), t1)

	t2, err := ParseJSONLossless([]byte("  {\"a\": 1}\n"))
	require.NoError(t, err)
	require.Equal(t, obj("a", num("1")), t2)

	t3, err := ParseJSONLossless([]byte("[]"))
	require.NoError(t, err)
	require.Equal(t, []any{}, t3)

	for _, bad := range []struct {
		name string
		in   string
		want error
	}{
		{"trailing garbage", `{"a":1} x`, ErrTrailingData},
		{"second document", `{"a":1}{"b":2}`, ErrTrailingData},
		{"empty input", "", ErrInvalidJSON},
		{"whitespace only", "  \n\t", ErrInvalidJSON},
		{"truncated", `{"a":`, ErrInvalidJSON},
		{"leading zero number", `{"a":01}`, ErrInvalidJSON},
		{"two scalars", `1 2`, ErrTrailingData},
	} {
		bad := bad
		t.Run(bad.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseJSONLossless([]byte(bad.in))
			require.ErrorIs(t, err, bad.want)
		})
	}
}

func TestParseJSONLosslessRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()
	// Mutation caught: decoding through a plain map would keep the
	// last duplicate and hide the conflict at every depth.
	for _, in := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"b":{"c":1,"c":2}}}`,
		`{"a":[{"k":1},{"k":2,"k":3}]}`,
	} {
		_, err := ParseJSONLossless([]byte(in))
		require.ErrorIs(t, err, ErrDuplicateKey, in)
	}
	_, err := ParseJSONLossless([]byte(`{"a":{},"b":{"a":1}}`))
	require.NoError(t, err) // same key at different depths is fine
}

func TestParseJSONLosslessKeepsNumberLexemes(t *testing.T) {
	t.Parallel()
	// Mutation caught: UseNumber-less decoding would round every
	// number through float64, losing "0.50" spelling and >2^53
	// digits; the type assertion is the point.
	tree, err := ParseJSONLossless(
		[]byte(`{"a":0.50,"b":1e2,"c":-0,"d":1E+2,"e":9223372036854775807}`))
	require.NoError(t, err)
	m := tree.(map[string]any)
	for k, want := range map[string]string{
		"a": "0.50", "b": "1e2", "c": "-0", "d": "1E+2",
		"e": "9223372036854775807",
	} {
		v, ok := m[k].(json.Number)
		require.True(t, ok, k)
		require.Equal(t, want, string(v), k)
	}
}

func TestParseJSONLosslessUTF8AndSurrogates(t *testing.T) {
	t.Parallel()
	// Mutation caught: a decoder that substitutes U+FFFD for invalid
	// bytes or lone surrogates would silently corrupt prompts and
	// secrets instead of refusing them.
	_, err := ParseJSONLossless([]byte("{\"a\":\"\xff\"}"))
	require.ErrorIs(t, err, ErrInvalidUTF8)

	for _, in := range []string{
		`{"a":"\ud800"}`,
		`{"a":"\udc00"}`,
		`{"a":"\ud800x"}`,
		`{"a":"\ud800\ud800"}`,
		`{"a":"x\udc00"}`,
	} {
		_, err := ParseJSONLossless([]byte(in))
		require.ErrorIs(t, err, ErrLoneSurrogate, in)
	}

	tree, err := ParseJSONLossless([]byte(`{"a":"\ud83d\ude00"}`))
	require.NoError(t, err)
	require.Equal(t, obj("a", "\U0001F600"), tree)

	tree, err = ParseJSONLossless([]byte(`{"a":"😀"}`))
	require.NoError(t, err)
	require.Equal(t, obj("a", "\U0001F600"), tree)
}

func TestParseJSONLosslessSurrogateSequenceBoundaries(t *testing.T) {
	t.Parallel()
	// Mutation caught: an index-advance bug after a valid pair
	// skipped the next four bytes, letting a lone surrogate hiding
	// behind a pair through; every case here exercises one
	// pair/lone ordering and the escaped-backslash path.
	for _, in := range []string{
		// Pair then lone high.
		`{"a":"😀\ud800"}`,
		// Pair then lone low.
		`{"a":"😀\udc00"}`,
		// Lone high then pair.
		`{"a":"\ud800😀"}`,
		// The same orderings with the pair written as JSON escapes:
		// these are the inputs that reach the scanner's pair branch.
		`{"a":"\ud83d\ude00\ud800"}`,
		`{"a":"\ud83d\ude00\udc00"}`,
		`{"a":"\ud800\ud83d\ude00"}`,
	} {
		_, err := ParseJSONLossless([]byte(in))
		require.ErrorIs(t, err, ErrLoneSurrogate, in)
	}

	// A pair at the very end of the string is accepted.
	tree, err := ParseJSONLossless([]byte(`{"a":"x😀"}`))
	require.NoError(t, err)
	require.Equal(t, obj("a", "x\U0001F600"), tree)

	// Raw JSON "\\ud800" is a literal backslash followed by the text
	// "ud800", not a surrogate escape; it must be accepted.
	tree, err = ParseJSONLossless([]byte(`{"a":"\\ud800"}`))
	require.NoError(t, err)
	require.Equal(t, obj("a", `\ud800`), tree)

	// Two escaped backslashes before "ud800" behave the same.
	tree, err = ParseJSONLossless([]byte(`{"a":"\\\\ud800"}`))
	require.NoError(t, err)
	require.Equal(t, obj("a", `\\ud800`), tree)
}

func TestEncodeKtavScalarRootRefused(t *testing.T) {
	t.Parallel()
	// Mutation caught: a writer accepting scalar roots would emit
	// documents no Ktav parser can read back (spec § 5.9.0).
	for _, root := range []Tree{
		num("42"), "str", true, nil, num("0"),
	} {
		_, err := EncodeKtav(root)
		require.ErrorIs(t, err, ErrScalarRoot, root)
	}
	_, err := EncodeKtav(obj("k", "v"))
	require.NoError(t, err)
	_, err = EncodeKtav([]any{num("1")})
	require.NoError(t, err)
}

func TestEncodeKtavExactCanonicalText(t *testing.T) {
	t.Parallel()
	// Mutation caught: any change to writer entry point, float
	// canonicalisation, raw-marker or quoting rules changes these
	// bytes; each line pins one engine behaviour.
	tests := []struct {
		name string
		tree Tree
		want string
	}{
		{"flat pair", obj("k", "v"), "k: v\n"},
		{"integer", obj("n", num("1")), "n: 1\n"},
		{
			"canonical float",
			obj("f", num("0.50")),
			"f: 0.5\n",
		},
		{
			"int64 max", obj("n", num("9223372036854775807")),
			"n: 9223372036854775807\n",
		},
		{"scalar-looking string", obj("s", "true"), "s :: true\n"},
		{"empty string", obj("s", ""), "s:\n"},
		{
			"nested object",
			obj("a", obj("b", "c")),
			"a: {\n    b: c\n}\n",
		},
		{
			"array of integers",
			obj("arr", []any{num("1"), num("2")}),
			"arr: [\n    1\n    2\n]\n",
		},
		{
			"empty containers", obj("a", obj(), "b", []any{}),
			"a: {}\nb: []\n",
		},
		{"hash-hash key quoted", obj("##x", "v"), "\"##x\": v\n"},
		{"dotted key quoted", obj("a.b", "v"), "\"a.b\": v\n"},
		{
			// Canonical prefers the verbatim ('((') multiline form:
			// chooseMultilineForm runs with preferStripped=false.
			"multiline verbatim",
			obj("k", "a\nb"),
			"k: ((\na\nb\n))\n",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := EncodeKtav(tt.tree)
			require.NoError(t, err)
			require.Equal(t, tt.want, string(out))
		})
	}
}

func TestEncodeKtavRefusals(t *testing.T) {
	t.Parallel()
	// Mutation caught: each refusal is a migration-safety property;
	// losing any one means silent corruption of configs, prompts or
	// secrets (design § 4.6).
	tests := []struct {
		name string
		tree Tree
		want error
	}{
		{"empty key", obj("", "v"), ErrEmptyKeyName},
		{"CR in string", obj("s", "a\rb"), ErrCRByte},
		{
			"CR in nested string",
			obj("a", []any{"ok", obj("s", "\r")}), ErrCRByte,
		},
		{
			"int64 max plus one",
			obj("n", num("9223372036854775808")), ErrInt64Range,
		},
		{
			"int64 min minus one",
			obj("n", num("-9223372036854775809")), ErrInt64Range,
		},
		{"overflow float", obj("f", num("1e400")), ErrFloatRange},
		{
			"value-changing decimal",
			obj("f", num("0.1000000000000000000000001")),
			ErrValueChangingDecimal,
		},
		{
			"trailing whitespace collision",
			obj("s", "a \n))"),
			ErrWhitespaceCollision,
		},
		{"both forms required", obj("s", ")\n))"), ErrBothFormsRequired},
		{
			"leading whitespace collision",
			obj("s", "\n  a\n  ))"), ErrWhitespaceCollision,
		},
		{"unsupported float64", obj("f", 1.5), ErrUnsupportedValue},
		{"unsupported int", obj("f", 1), ErrUnsupportedValue},
		// NaN and infinities fall through validateTree's default
		// branch (the document model has no float64 numbers), so the
		// sentinel is ErrUnsupportedValue, not ErrNonFiniteFloat.
		{"NaN", obj("f", math.NaN()), ErrUnsupportedValue},
		{"+Inf", obj("f", math.Inf(1)), ErrUnsupportedValue},
		{"-Inf", obj("f", math.Inf(-1)), ErrUnsupportedValue},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := EncodeKtav(tt.tree)
			require.ErrorIs(t, err, tt.want)
			require.Nil(t, out)
		})
	}
}

func TestEncodeKtavInt64BoundariesEncode(t *testing.T) {
	t.Parallel()
	// Mutation caught: an off-by-one range check would refuse legal
	// boundary integers or accept the neighbours outside int64.
	for _, n := range []string{
		"9223372036854775807", "-9223372036854775808", "0", "-0",
	} {
		out, err := ConvertJSONToKtav([]byte(`{"n":` + n + `}`))
		require.NoError(t, err, n)
		tree, err := DecodeKtav(out)
		require.NoError(t, err, n)
		require.NoError(t, Equivalent(obj("n", num(n)), tree), n)
	}
}

func TestConvertJSONToKtavRefusesInsteadOfLossy(t *testing.T) {
	t.Parallel()
	// Mutation caught: a converter that pushed lossy values through
	// would round >binary64 decimals or overflow integers silently.
	for _, in := range []string{
		`{"f":0.1000000000000000000000001}`,
		`{"n":9223372036854775808}`,
		`{"f":1e400}`,
		`{"s":"a\rb"}`,
		`{"":"v"}`,
	} {
		out, err := ConvertJSONToKtav([]byte(in))
		require.Error(t, err, in)
		require.Nil(t, out, in)
	}
}

func TestConvertJSONToKtavNumericRangeRefusals(t *testing.T) {
	t.Parallel()
	// Mutation caught: 1e400 overflows binary64 (ParseFloat reports
	// ErrRange) and 1e-400 underflows silently to zero (nil error), so
	// the exact-value check must refuse it; a converter accepting
	// either saturates or zeroes the value.
	out, err := ConvertJSONToKtav([]byte(`{"f":1e400}`))
	require.ErrorIs(t, err, ErrFloatRange)
	require.Nil(t, out)
	out, err = ConvertJSONToKtav([]byte(`{"f":1e-400}`))
	require.ErrorIs(t, err, ErrValueChangingDecimal)
	require.Nil(t, out)

	// A decoded CR (here escaped as \r inside the JSON string) is
	// refused: no Ktav form can carry it.
	out, err = ConvertJSONToKtav([]byte(`{"s":"a\r\nb"}`))
	require.ErrorIs(t, err, ErrCRByte)
	require.Nil(t, out)
}

func TestConvertJSONToKtavBigIntegerExactText(t *testing.T) {
	t.Parallel()
	// Mutation caught: numbers beyond the float64-safe integer range
	// must survive conversion byte-exact; the writer's integer
	// branch (engine ktav_wire.go taggedI) emits the lexeme
	// verbatim, so the exact Ktav text is pinned here.
	for _, tt := range []struct {
		in   string
		want string
	}{
		{"{\"n\":9007199254740993}", "n: 9007199254740993\n"},
		{"{\"n\":9223372036854775807}", "n: 9223372036854775807\n"},
		{"{\"n\":-9223372036854775808}", "n: -9223372036854775808\n"},
	} {
		out, err := ConvertJSONToKtav([]byte(tt.in))
		require.NoError(t, err, tt.in)
		require.Equal(t, tt.want, string(out), tt.in)
	}
}

func roundTrip(t *testing.T, key, value string) {
	t.Helper()
	tree, err := ParseJSONLossless(
		[]byte(`{` + key + `:` + value + `}`))
	require.NoError(t, err)
	out, err := EncodeKtav(tree)
	require.NoError(t, err)
	back, err := DecodeKtav(out)
	require.NoError(t, err)
	require.NoError(t, Equivalent(tree, back), "ktav bytes:\n%s", out)
}

func TestRoundTripScalarLookingStrings(t *testing.T) {
	t.Parallel()
	// Mutation caught: misclassifying scalar-looking strings as
	// booleans/null/numbers (or vice versa) rewrites provider
	// settings silently; each case survives only with the raw
	// marker the writer owns.
	for _, s := range []string{
		"true", "false", "null", "0", "01", "1.5", "1e3", "", "  padded  ",
		"+1", "-1.0e10", "0x1f", "1_000",
	} {
		roundTrip(t, `"k"`, marshalString(s))
	}
}

func TestRoundTripEnvironmentTemplatesByteIdentical(t *testing.T) {
	t.Parallel()
	// Mutation caught: any migration-time interpretation of $ or
	// backslash sequences would execute or rewrite shell templates;
	// the string content must survive byte-identical.
	for _, s := range []string{
		"$VAR", "${X:-y}", "$(cmd)", `C:\Users\rush\config.json`,
		`^\d{3}-\d{4}$`, `a\\b\", \"c`, "%HOME%\\bin", "a:b", "a b",
		"((", "))", "text with ) paren", "text with (( paren",
	} {
		roundTrip(t, `"k"`, marshalString(s))
	}
}

// marshalString renders a Go string as one JSON string literal.
func marshalString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestRoundTripMultilineStrings(t *testing.T) {
	t.Parallel()
	// Mutation caught: multiline form selection is where prompts
	// lose blank lines, trailing newlines or edge spaces; every
	// shape here exercises a different branch of that selection.
	for _, s := range []string{
		"line1\nline2", "a\n\nb", "trailing\n", "\nleading",
		"lead\nmiddle\n\n", " spaces keep ", "tab\tinside",
		")\n)", ")))",
	} {
		roundTrip(t, `"k"`, marshalString(s))
	}
}

func TestRoundTripKeys(t *testing.T) {
	t.Parallel()
	// Mutation caught: dotted or delimiter keys must never become
	// dotted paths (a nested-object rewrite), and quoted-key
	// emission must round trip every shape.
	for _, k := range []string{
		"a.b", "a:b", "a b", `a\b`, "ключ", "42", "##x", "a#",
		"trailing ", " leading", "[bracket]", "{brace}", "a,b",
		"(paren)", "quoted\"key", "tab\tkey",
	} {
		tree, err := ParseJSONLossless(
			[]byte(`{` + marshalString(k) + `:"v"}`))
		require.NoError(t, err)
		out, err := EncodeKtav(tree)
		require.NoError(t, err, k)
		back, err := DecodeKtav(out)
		require.NoError(t, err, k)
		require.NoError(t, Equivalent(tree, back), "key %q ktav:\n%s",
			k, out)
	}
}

func TestRoundTripContainers(t *testing.T) {
	t.Parallel()
	// Mutation caught: nested arrays of objects with order
	// significance model provider model lists; an order-insensitive
	// or shape-collapsing codec would reorder them silently.
	tree := obj(
		"models", []any{
			obj("name", "m1", "opts", obj("temperature", num("0.5"))),
			obj("name", "m2", "tags", []any{}),
		},
		"empty_obj", obj(),
		"empty_arr", []any{},
		"nested", []any{[]any{[]any{num("1")}}},
	)
	out, err := EncodeKtav(tree)
	require.NoError(t, err)
	back, err := DecodeKtav(out)
	require.NoError(t, err)
	require.NoError(t, Equivalent(tree, back))

	// Array order is semantic: swapping elements must differ.
	swapped := obj("models", []any{
		obj("name", "m2", "tags", []any{}),
		obj("name", "m1", "opts", obj("temperature", num("0.5"))),
	})
	require.Error(t, Equivalent(tree, swapped))
}

func TestRoundTripPresence(t *testing.T) {
	t.Parallel()
	// Mutation caught: conflating null, false, 0 and "" collapses
	// explicit settings into defaults (design § 5.1 omitempty trap).
	values := []Tree{nil, false, num("0"), "", obj(), []any{}}
	for _, a := range values {
		for _, b := range values {
			ta := obj("k", a)
			tb := obj("k", b)
			if sameTree(a, b) {
				require.NoError(t, Equivalent(ta, tb))
			} else {
				require.Error(t, Equivalent(ta, tb))
			}
		}
	}

	// Each presence value must survive an actual encode/decode round
	// trip unchanged: collapsing null to "", 0 to null and so on
	// would rewrite explicit settings into defaults.
	for _, v := range values {
		tree := obj("k", v)
		out, err := EncodeKtav(tree)
		require.NoError(t, err)
		back, err := DecodeKtav(out)
		require.NoError(t, err)
		require.NoError(t, Equivalent(tree, back), "ktav:\n%s", out)
	}
}

// sameTree reports whether two singleton trees hold the same value.
func sameTree(a, b Tree) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && av == bv
	case map[string]any:
		_, ok := b.(map[string]any)
		return ok && len(av) == 0
	case []any:
		_, ok := b.([]any)
		return ok && len(av) == 0
	default:
		return false
	}
}

func TestEquivalentValueBasedNumbers(t *testing.T) {
	t.Parallel()
	// Mutation caught: float64 comparison would equate distinct
	// decimals beyond 2^53 and separate equal ones by spelling;
	// math/big comparison must accept spelling changes but reject
	// value changes.
	require.NoError(t, Equivalent(num("1e2"), num("100")))
	require.NoError(t, Equivalent(num("0.50"), num("0.5")))
	require.NoError(t, Equivalent(num("1"), num("1.0")))
	require.NoError(t, Equivalent(num("-0"), num("0")))
	require.NoError(t, Equivalent(
		num("9007199254740993"), num("9007199254740993")))
	require.Error(t, Equivalent(num("9007199254740993"),
		num("9007199254740992")))
	require.Error(t, Equivalent(num("1"), num("2")))

	// Path named, values never printed. JSON-pointer style: $, then
	// /-separated segments.
	err := Equivalent(obj("a", obj("b", []any{num("1"), num("2")})),
		obj("a", obj("b", []any{num("1"), num("3")})))
	require.ErrorIs(t, err, ErrNotEquivalent)
	require.Contains(t, err.Error(), "$/a/b/1")
	require.NotContains(t, err.Error(), "9")
}

func TestEquivalentDeterministicSortedFirstDifference(t *testing.T) {
	t.Parallel()
	// Mutation caught: map iteration order would make the reported
	// first difference random between runs, and a missing/extra
	// member would be reported at the parent path instead of the
	// offending key.
	a := obj("z", num("1"), "a", obj("m", "x"), "b", true)
	b := obj("z", num("2"), "a", obj("m", "y"), "b", false)
	for i := 0; i < 20; i++ {
		err := Equivalent(a, b)
		require.ErrorIs(t, err, ErrNotEquivalent)
		// Sorted keys are a, b, z; the first difference is always a.
		require.Contains(t, err.Error(), "$/a", err)
	}

	// A missing member is named in the path.
	err := Equivalent(obj("keep", num("1"), "gone", num("2")),
		obj("keep", num("1")))
	require.ErrorIs(t, err, ErrNotEquivalent)
	require.Contains(t, err.Error(), "$/gone", err)

	// Key "a.b" and nested a → b render unambiguously apart.
	err = Equivalent(obj("a.b", "x"), obj("a", obj("b", "x")))
	require.ErrorIs(t, err, ErrNotEquivalent)
	require.Contains(t, err.Error(), "$/a.b", err)
	require.NotContains(t, err.Error(), "$/a/b", err)
}

func TestEquivalentStructure(t *testing.T) {
	t.Parallel()
	// Mutation caught: membership, missing-vs-null and array-order
	// checks are the semantic contract Equivalent sells to the
	// migration lane.
	require.Error(t, Equivalent(obj("a", nil), obj("b", nil)))
	require.Error(t, Equivalent(obj("a", nil), obj()))
	require.Error(t, Equivalent(obj("a", "x"), obj("a", "y")))
	require.Error(t, Equivalent(obj("a", true), obj("a", false)))
	require.Error(t, Equivalent(obj(), []any{}))
	require.Error(t, Equivalent([]any{num("1"), num("2")},
		[]any{num("2"), num("1")}))
	require.NoError(t, Equivalent(obj(), obj()))
	require.NoError(t, Equivalent([]any{}, []any{}))
}

func TestDecodeKtavStrict(t *testing.T) {
	t.Parallel()
	// Mutation caught: a lax decode would accept lossy scalars the
	// strict parser refuses, breaking the round-trip proof that
	// ConvertJSONToKtav sells.
	tree, err := DecodeKtav([]byte("k: v\n"))
	require.NoError(t, err)
	require.Equal(t, obj("k", "v"), tree)

	_, err = DecodeKtav([]byte("{"))
	require.Error(t, err)

	// Numbers decode back into json.Number, not float64.
	tree, err = DecodeKtav([]byte("n: 1\nf: 0.5\n"))
	require.NoError(t, err)
	m := tree.(map[string]any)
	_, ok := m["n"].(json.Number)
	require.True(t, ok)
	_, ok = m["f"].(json.Number)
	require.True(t, ok)
	require.Equal(t, "1", string(m["n"].(json.Number)))

	// Strict-only rejection: a trailing-zero float parses lax but is
	// LossyScalar under LoadsStrict (the corpus strict-lossy
	// trailing_zero fixture pins the same class). Swapping
	// LoadsStrict for lax Loads makes this assertion fail.
	_, err = DecodeKtav([]byte("n: 0.50\n"))
	require.Error(t, err)
	require.ErrorContains(t, err, "LossyScalar")
	require.NotContains(t, err.Error(), "0.50")
}

func TestToJSONDeterministic(t *testing.T) {
	t.Parallel()
	// Mutation caught: HTML escaping or unsorted keys would make
	// fingerprints unstable between runs and libraries.
	out, err := ToJSON(obj("b", num("1"), "a", num("2")))
	require.NoError(t, err)
	require.Equal(t, "{\"a\":2,\"b\":1}\n", string(out))

	out, err = ToJSON(obj("s", "<&>"))
	require.NoError(t, err)
	require.Equal(t, "{\"s\":\"<&>\"}\n", string(out))
	require.NotContains(t, string(out), "\\u003c")
}

func TestConvertJSONToKtavEndToEnd(t *testing.T) {
	t.Parallel()
	// Mutation caught: the converter is the migration lane's entry
	// point; a broken link in the parse-encode-decode-compare chain
	// surfaces here first.
	in := []byte(`{
		"provider": "openai",
		"prompt": "line1\nline2",
		"big": 9223372036854775807,
		"half": 0.50,
		"flags": {"on": true, "off": false, "none": null},
		"list": [1, "two", {"three": 3}],
		"empty": {},
		"unicode": "ключ 😀"
	}`)
	out, err := ConvertJSONToKtav(in)
	require.NoError(t, err)
	require.NotEmpty(t, out)
	back, err := DecodeKtav(out)
	require.NoError(t, err)
	want, err := ParseJSONLossless(in)
	require.NoError(t, err)
	require.NoError(t, Equivalent(want, back))

	// Ktav text in, JSON text out, both deterministic.
	j, err := ToJSON(back)
	require.NoError(t, err)
	reparsed, err := ParseJSONLossless(j)
	require.NoError(t, err)
	require.NoError(t, Equivalent(want, reparsed))
}

func TestErrorMessagesNeverContainValues(t *testing.T) {
	t.Parallel()
	// Mutation caught: diagnostics leaking secret-bearing values is
	// an explicit acceptance criterion (design § 8).
	_, err := ParseJSONLossless([]byte(`{"a":1,"a":2}`))
	require.ErrorContains(t, err, "duplicate")
	require.NotContains(t, err.Error(), "2")

	err = Equivalent(obj("secret_token", "hunter2"),
		obj("secret_token", "hunter3"))
	require.ErrorIs(t, err, ErrNotEquivalent)
	require.Contains(t, err.Error(), "secret_token")
	require.NotContains(t, err.Error(), "hunter")

	out, err := EncodeKtav(obj("secret", "a\rb"))
	require.ErrorIs(t, err, ErrCRByte)
	require.Nil(t, out)
	require.NotContains(t, err.Error(), "a\rb")
	require.True(t, errors.Is(err, ErrCRByte))
	require.False(t, strings.Contains(err.Error(), "\r"))

	// DecodeKtav wraps engine refusals from structure only: the
	// strict parser's LossyScalar payload embeds the numeric body
	// ("12345.00") and its canonical form, neither of which may
	// reach the message. The class does.
	_, err = DecodeKtav([]byte("secret: 12345.00\n"))
	require.Error(t, err)
	require.ErrorContains(t, err, "LossyScalar")
	require.NotContains(t, err.Error(), "12345")
	require.NotContains(t, err.Error(), "1.2345")

	// EncodeKtav refusals never print the value either; the key path
	// is structure and stays (see the joinPath doc comment).
	_, err = EncodeKtav(obj("k", "s3cret-value\r"))
	require.ErrorIs(t, err, ErrCRByte)
	require.NotContains(t, err.Error(), "s3cret")
	require.True(t, errors.Is(err, ErrCRByte))

	// ConvertJSONToKtav inherits the same guarantee: a duplicate-key
	// refusal names no key or value text.
	_, err = ConvertJSONToKtav(
		[]byte(`{"s3cret":"v1","s3cret":"v2"}`))
	require.ErrorIs(t, err, ErrDuplicateKey)
	require.NotContains(t, err.Error(), "s3cret")
	require.NotContains(t, err.Error(), "v1")
	require.NotContains(t, err.Error(), "v2")
}
