// Package ktavcodec converts Rush-owned configuration documents between
// JSON and Ktav 0.8.1 without semantic loss. It is the only place in
// the repository allowed to import a Ktav package, and the only
// permitted import is the pure, standard-library-only backend
// github.com/ktav-lang/golang/pure.
//
// Tree is the codec's document model. It is the ordinary JSON-shaped
// any: map[string]any, []any, string, bool, nil, and exactly one
// number type, json.Number. json.Number keeps the source lexeme
// (no float64 round trip), so callers can validate exact numeric
// values and Equivalent can compare them through math/big.Rat.
//
// All sentinel errors in this package are comparable with errors.Is.
// Error messages never contain document values, only structure.
package ktavcodec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ktav-lang/golang/pure"
)

// Tree is a JSON-shaped document value. Numbers are always
// json.Number.
type Tree = any

// Sentinel errors reported by this package. Use errors.Is.
var (
	// ErrInvalidJSON reports malformed JSON input.
	ErrInvalidJSON = errors.New("ktavcodec: invalid JSON")
	// ErrInvalidUTF8 reports input that is not well-formed UTF-8.
	ErrInvalidUTF8 = errors.New("ktavcodec: input is not valid UTF-8")
	// ErrLoneSurrogate reports a \\uD800-\\uDFFF escape that is not
	// one half of a well-formed surrogate pair.
	ErrLoneSurrogate = errors.New("ktavcodec: lone surrogate escape")
	// ErrDuplicateKey reports the same decoded object key twice at
	// any depth.
	ErrDuplicateKey = errors.New("ktavcodec: duplicate object key")
	// ErrTrailingData reports data after the single JSON document.
	ErrTrailingData = errors.New("ktavcodec: trailing data after document")
	// ErrScalarRoot reports a document root that is not an object or
	// an array (spec § 5.9.0).
	ErrScalarRoot = errors.New("ktavcodec: scalar document root")
	// ErrEmptyKeyName reports an object key that is the empty string
	// (spec § 5.9.0).
	ErrEmptyKeyName = errors.New("ktavcodec: empty object key")
	// ErrCRByte reports a string containing a CR byte (spec
	// § 5.9.7), which no Ktav form can represent.
	ErrCRByte = errors.New("ktavcodec: string contains CR byte")
	// ErrNonFiniteFloat reports a NaN or infinite float.
	ErrNonFiniteFloat = errors.New("ktavcodec: non-finite float")
	// ErrInt64Range reports an integer literal outside the int64
	// domain, which the strict round trip would not preserve.
	ErrInt64Range = errors.New("ktavcodec: integer outside int64 range")
	// ErrFloatRange reports a float literal that overflows binary64.
	ErrFloatRange = errors.New("ktavcodec: float outside binary64 range")
	// ErrValueChangingDecimal reports a decimal literal whose value
	// would change when stored as a binary64 float.
	ErrValueChangingDecimal = errors.New(
		"ktavcodec: decimal value would change under binary64")
	// ErrWhitespaceCollision reports a multiline string neither
	// multiline form can reproduce (spec § 5.9.7).
	ErrWhitespaceCollision = errors.New(
		"ktavcodec: multiline whitespace collision")
	// ErrBothFormsRequired reports a multiline string needing both
	// '(' and '((' segments (spec § 5.9.7).
	ErrBothFormsRequired = errors.New(
		"ktavcodec: string needs both multiline forms")
	// ErrUnsupportedValue reports a Tree holding a Go type outside
	// the documented document model.
	ErrUnsupportedValue = errors.New("ktavcodec: unsupported value type")
	// ErrNotEquivalent reports two trees that differ; the message
	// names the JSON path of the first difference.
	ErrNotEquivalent = errors.New("ktavcodec: trees differ")
)

// ParseJSONLossless decodes exactly one complete JSON document into a
// Tree. It rejects trailing data, duplicate decoded object keys at
// every depth, invalid UTF-8 bytes, and lone surrogate escapes. All
// numbers are returned as json.Number carrying the source lexeme
// byte-for-byte; no value passes through float64.
func ParseJSONLossless(b []byte) (Tree, error) {
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%w", ErrInvalidUTF8)
	}
	if err := scanSurrogateEscapes(b); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	t, err := decodeTree(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w", ErrTrailingData)
		}
		return nil, fmt.Errorf("%w: %w", ErrTrailingData, err)
	}
	return t, nil
}

// decodeTree reads one JSON value through the token stream, building
// maps and slices itself so duplicate keys are caught at every depth.
func decodeTree(dec *json.Decoder) (Tree, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	return decodeTreeToken(dec, tok)
}

func decodeTreeToken(dec *json.Decoder, tok json.Token) (Tree, error) {
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			return decodeObject(dec)
		case '[':
			return decodeArray(dec)
		default:
			return nil, fmt.Errorf("%w: unexpected delimiter %q",
				ErrInvalidJSON, d)
		}
	}
	switch t := tok.(type) {
	case nil, bool, string, json.Number:
		return t, nil
	default:
		return nil, fmt.Errorf("%w: unexpected token %T",
			ErrInvalidJSON, tok)
	}
}

func decodeObject(dec *json.Decoder) (Tree, error) {
	obj := map[string]any{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: non-string object key",
				ErrInvalidJSON)
		}
		if _, dup := obj[key]; dup {
			return nil, fmt.Errorf("%w", ErrDuplicateKey)
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
		}
		val, err := decodeTreeToken(dec, valTok)
		if err != nil {
			return nil, err
		}
		obj[key] = val
	}
	if _, err := dec.Token(); err != nil { // consume '}'
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	return obj, nil
}

func decodeArray(dec *json.Decoder) (Tree, error) {
	arr := []any{}
	for dec.More() {
		val, err := decodeTree(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	if _, err := dec.Token(); err != nil { // consume ']'
		return nil, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	return arr, nil
}

// scanSurrogateEscapes walks raw JSON bytes tracking string context
// and rejects \\uD800-\\uDFFF escapes that do not form a well-formed
// pair. Literal invalid UTF-8 is rejected before this runs.
func scanSurrogateEscapes(b []byte) error {
	const (
		outside = iota
		inside
	)
	state := outside
	// Every loop iteration advances i by exactly the bytes it consumed
	// (a pair consumes both escapes in one step), so no escape is ever
	// skipped and re-scanned.
	for i := 0; i < len(b); {
		c := b[i]
		if state == outside {
			if c == '"' {
				state = inside
			}
			i++
			continue
		}
		switch c {
		case '"':
			state = outside
			i++
		case '\\':
			if i+1 >= len(b) {
				return fmt.Errorf("%w", ErrInvalidJSON)
			}
			if b[i+1] != 'u' {
				// Escaped character: "\\ud800" in the raw bytes is a
				// literal backslash followed by the text "ud800", so it
				// must not be inspected as a \\u escape at all.
				i += 2
				continue
			}
			hi, ok := hex4(b, i+2)
			if !ok {
				// Malformed \\u: the JSON decoder owns the refusal.
				i += 2
				continue
			}
			if hi >= 0xDC00 && hi <= 0xDFFF {
				return fmt.Errorf("%w: unpaired low surrogate escape",
					ErrLoneSurrogate)
			}
			if hi >= 0xD800 && hi <= 0xDBFF {
				if i+1+6 > len(b) || b[i+6] != '\\' || i+7 >= len(b) ||
					b[i+7] != 'u' {
					return fmt.Errorf("%w: unpaired high surrogate escape",
						ErrLoneSurrogate)
				}
				lo, ok := hex4(b, i+8)
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return fmt.Errorf("%w: unpaired high surrogate escape",
						ErrLoneSurrogate)
				}
				i += 12 // both escapes consumed together
				continue
			}
			i += 6 // the single \\uDXXX escape
		default:
			i++
		}
	}
	return nil
}

func hex4(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	var v rune
	for _, c := range b[i : i+4] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// EncodeKtav writes a Tree as Ktav through the pure backend. It uses
// pure.EmitCanonical, not Dumps or DumpsForceStrings, for three
// reasons, each anchored in the engine source:
//
//   - DumpsForceStrings (pure.go:14, engine_pure.go:34) coerces every
//     leaf scalar to its textual form (engine_pure_write.go
//     forcedStringText), turning numbers into strings. The codec must
//     never silently convert a number into a string, so it is out.
//   - Between Dumps and EmitCanonical both sort object keys
//     alphabetically (engine_ktav_wire.go prepareWriterValue sorts
//     each map before writing), so ordering determinism does not
//     distinguish them.
//   - EmitCanonical renders floats through the § 5.9.8 canonical form
//     (engine_pure.go pureEmitCanonicalTagged -> writer modeCanonical;
//     engine_pure_num.go canonicalFloatText), while pretty Dumps
//     passes each float payload through verbatim (engine_pure_write.go
//     kFloat branches). Canonical output is therefore a pure function
//     of the value, stable for fingerprints and byte comparison, and
//     it refuses multiline strings no form can represent
//     (chooseMultilineForm with preferStripped=false) instead of
//     emitting a lossy verbatim form the way pretty mode does.
//
// Refusals are typed sentinel errors; see the Err* variables. The
// engine's own refusals are mapped onto them.
func EncodeKtav(t Tree) ([]byte, error) {
	if err := validateTree(t, nil); err != nil {
		return nil, err
	}
	out, err := pure.EmitCanonical(t)
	if err != nil {
		return nil, wrapEngineError(err)
	}
	return []byte(out), nil
}

// validateTree pre-checks the documented document model and the
// refusals the codec owns, so callers get typed sentinels before the
// engine runs.
func validateTree(t Tree, path []string) error {
	switch v := t.(type) {
	case nil, bool, string:
		if s, ok := t.(string); ok && strings.IndexByte(s, '\r') >= 0 {
			return fmt.Errorf("%w at %s", ErrCRByte, joinPath(path))
		}
		return nil
	case json.Number:
		return validateNumber(string(v), path)
	case int64:
		return nil
	case map[string]any:
		for k, child := range v {
			if k == "" {
				return fmt.Errorf("%w at %s", ErrEmptyKeyName,
					joinPath(append(path, k)))
			}
			if err := validateTree(child, append(path, k)); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, child := range v {
			if err := validateTree(child, path); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %T at %s", ErrUnsupportedValue,
			t, joinPath(path))
	}
}

// validateNumber enforces the numeric domain the strict round trip
// preserves: integers within int64, finite binary64 floats whose
// value the shortest-round-trip text reproduces exactly.
func validateNumber(s string, path []string) error {
	if s == "" {
		return fmt.Errorf("%w: empty number lexeme at %s", ErrInvalidJSON,
			joinPath(path))
	}
	if isIntegerLexeme(s) {
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return fmt.Errorf("%w at %s", ErrInt64Range, joinPath(path))
		}
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%w at %s", ErrFloatRange, joinPath(path))
	}
	src := new(big.Rat)
	if _, ok := src.SetString(s); !ok {
		return fmt.Errorf("%w at %s", ErrInvalidJSON, joinPath(path))
	}
	round := new(big.Rat)
	if _, ok := round.SetString(strconv.FormatFloat(f, 'g', -1, 64)); !ok {
		return fmt.Errorf("%w at %s", ErrInvalidJSON, joinPath(path))
	}
	if src.Cmp(round) != 0 {
		return fmt.Errorf("%w at %s", ErrValueChangingDecimal,
			joinPath(path))
	}
	return nil
}

// isIntegerLexeme reports whether s is a base-10 integer literal.
func isIntegerLexeme(s string) bool {
	body := strings.TrimPrefix(s, "-")
	if body == "" {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	return true
}

// wrapEngineError maps the pure backend's structured refusals onto
// this package's sentinels. The *pure.Error stays reachable through
// errors.As, but its message text is rebuilt from structure only:
// Msg, Body, Canonical, LineText and Path all carry document content
// and are never printed (Path holds decoded keys, i.e. document
// content, so it is omitted from the message while the field remains
// set on the attached *pure.Error for callers that need it).
func wrapEngineError(err error) error {
	var pe *pure.Error
	if !errors.As(err, &pe) {
		return fmt.Errorf("ktav encode: %w", err)
	}
	var sentinel error
	switch pe.Reason {
	case "ScalarRoot":
		sentinel = ErrScalarRoot
	case "EmptyKeyName":
		sentinel = ErrEmptyKeyName
	case "CRByte":
		sentinel = ErrCRByte
	case "NonFiniteFloat":
		sentinel = ErrNonFiniteFloat
	case "BothFormsRequired":
		sentinel = ErrBothFormsRequired
	case "TrailingWhitespaceCollision", "LeadingWhitespaceCollision":
		sentinel = ErrWhitespaceCollision
	}
	return newRedactedError("ktav encode", sentinel, pe)
}

// redactedError renders an engine refusal from structure only. The
// attached *pure.Error stays unwrappable (errors.Is on the sentinel,
// errors.As on *pure.Error both work) but its value-bearing fields
// never reach the message.
type redactedError struct {
	context  string
	sentinel error
	pe       *pure.Error
}

func newRedactedError(context string, sentinel error, pe *pure.Error) error {
	return &redactedError{context: context, sentinel: sentinel, pe: pe}
}

func (e *redactedError) Error() string {
	var b strings.Builder
	b.WriteString(e.context)
	if e.sentinel != nil {
		b.WriteString(": ")
		b.WriteString(e.sentinel.Error())
	}
	b.WriteString(": ")
	b.WriteString(e.pe.Class)
	if e.pe.Reason != "" {
		b.WriteString(" (")
		b.WriteString(e.pe.Reason)
		b.WriteString(")")
	}
	if e.pe.Line > 0 {
		fmt.Fprintf(&b, " at line %d", e.pe.Line)
	}
	if e.pe.SpecSection != "" {
		b.WriteString(" ")
		b.WriteString(e.pe.SpecSection)
	}
	return b.String()
}

func (e *redactedError) Unwrap() []error {
	if e.sentinel != nil {
		return []error{e.sentinel, e.pe}
	}
	return []error{e.pe}
}

// DecodeKtav strictly parses Ktav bytes (pure.LoadsStrict) into a
// Tree using the same number representation as ParseJSONLossless, so
// Equivalent can compare the two.
func DecodeKtav(b []byte) (Tree, error) {
	v, err := pure.LoadsStrict(string(b))
	if err != nil {
		var pe *pure.Error
		if errors.As(err, &pe) {
			return nil, newRedactedError("ktav decode", nil, pe)
		}
		return nil, fmt.Errorf("ktav decode: %w", err)
	}
	return fromPure(v), nil
}

// fromPure converts the pure backend's Go shapes into Tree values.
// Integers arrive as int64 (or *big.Int for oversized payloads) and
// floats as float64 carrying the shortest-round-trip value; both are
// re-tokenised as json.Number lexemes.
func fromPure(v any) Tree {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = fromPure(child)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = fromPure(child)
		}
		return out
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case *big.Int:
		return json.Number(t.String())
	case big.Int:
		return json.Number(t.String())
	case float64:
		return json.Number(strconv.FormatFloat(t, 'g', -1, 64))
	default:
		return t
	}
}

// ToJSON serialises a Tree deterministically: object keys sorted,
// HTML escaping disabled, one trailing newline. Encoding/json already
// sorts string map keys, which together with json.Number passthrough
// makes the output a pure function of the value.
func ToJSON(t Tree) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(t); err != nil {
		return nil, fmt.Errorf("ktavcodec: encode JSON: %w", err)
	}
	return buf.Bytes(), nil
}

// Equivalent reports whether two Trees are semantically equal: same
// object membership and key strings, same string content, same array
// order, same bool/null, empty containers equal only to themselves,
// and numbers equal by exact mathematical value through math/big.Rat
// (never float64). The returned error wraps ErrNotEquivalent and
// names the JSON path of the first difference without printing any
// value.
func Equivalent(a, b Tree) error {
	var path []string
	if !equivalentAt(a, b, &path) {
		return fmt.Errorf("%w at %s", ErrNotEquivalent, joinPath(path))
	}
	return nil
}

// equivalentAt compares one position, recording the path of the first
// difference in *path.
func equivalentAt(a, b Tree, path *[]string) bool {
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
		bn, ok := numberOf(b)
		return ok && ratOf(av).Cmp(ratOf(bn)) == 0
	case int64:
		bn, ok := numberOf(b)
		return ok && ratOf(json.Number(strconv.FormatInt(av, 10))).
			Cmp(ratOf(bn)) == 0
	case map[string]any:
		bm, ok := b.(map[string]any)
		if !ok {
			return false
		}
		// Keys are visited in sorted order so the reported first
		// difference is deterministic, and a missing or extra member
		// is named in the path.
		if len(av) != len(bm) {
			for _, k := range sortedMapKeys(av) {
				if _, exists := bm[k]; !exists {
					*path = append(*path, k)
					return false
				}
			}
			for _, k := range sortedMapKeys(bm) {
				if _, exists := av[k]; !exists {
					*path = append(*path, k)
					return false
				}
			}
			return false
		}
		for _, k := range sortedMapKeys(av) {
			bv, exists := bm[k]
			*path = append(*path, k)
			if !exists || !equivalentAt(av[k], bv, path) {
				return false
			}
			*path = (*path)[:len(*path)-1]
		}
		return true
	case []any:
		bs, ok := b.([]any)
		if !ok || len(av) != len(bs) {
			return false
		}
		for i := range av {
			*path = append(*path, strconv.Itoa(i))
			if !equivalentAt(av[i], bs[i], path) {
				return false
			}
			*path = (*path)[:len(*path)-1]
		}
		return true
	default:
		return false
	}
}

// numberOf accepts the documented number types.
func numberOf(v any) (json.Number, bool) {
	switch t := v.(type) {
	case json.Number:
		return t, true
	case int64:
		return json.Number(strconv.FormatInt(t, 10)), true
	default:
		return "", false
	}
}

// ratOf converts a number lexeme to its exact rational value.
func ratOf(n json.Number) *big.Rat {
	r, ok := new(big.Rat).SetString(string(n))
	if !ok {
		// Unreachable for lexemes that passed validateTree or the
		// JSON parser; kept total so Equivalent never panics.
		return new(big.Rat)
	}
	return r
}

// ConvertJSONToKtav is the entry point later migration lanes call:
// parse the JSON losslessly, encode Ktav, strictly parse the result
// back, and prove semantic equality. Any failing step aborts with an
// error and no output bytes.
func ConvertJSONToKtav(jsonBytes []byte) ([]byte, error) {
	t, err := ParseJSONLossless(jsonBytes)
	if err != nil {
		return nil, err
	}
	out, err := EncodeKtav(t)
	if err != nil {
		return nil, err
	}
	back, err := DecodeKtav(out)
	if err != nil {
		return nil, err
	}
	if err := Equivalent(t, back); err != nil {
		return nil, fmt.Errorf("ktav convert: %w", err)
	}
	return out, nil
}

// joinPath renders a JSON-pointer style path (RFC 6901): root "$",
// then one "/"-prefixed segment per key or index, with "~" and "/"
// escaped as "~0" and "~1" so the key "a.b" is unambiguously distinct
// from a nested a → b. Keys are structure, not secrets in Rush
// configs, so key paths stay in messages; document values never do.
func joinPath(path []string) string {
	var b strings.Builder
	b.WriteByte('$')
	for _, seg := range path {
		b.WriteByte('/')
		b.WriteString(escapePointerSegment(seg))
	}
	return b.String()
}

// sortedMapKeys returns m's keys in sorted order.
func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// escapePointerSegment applies RFC 6901 ~0/~1 escaping.
func escapePointerSegment(seg string) string {
	s := strings.ReplaceAll(seg, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}
