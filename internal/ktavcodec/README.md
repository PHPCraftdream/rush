# internal/ktavcodec

Lossless JSON ⇄ Ktav 0.8.1 codec for Rush-owned configuration documents.
This is the K0 lane deliverable: later migration lanes call
`ConvertJSONToKtav` and nothing else.

## Import rule (repository-wide)

The **only** Ktav import allowed anywhere in the repository is
`github.com/ktav-lang/golang/pure`. The root package
`github.com/ktav-lang/golang` (purego C-ABI wrapper), its `internal/*`
(including the native backend), and `github.com/ebitengine/purego` are
forbidden in all code, tests included. The gate lives in `.golangci.yml`
(depguard) and `.github/workflows/ktav-purity.yml`. `pure` is
standard-library-only; public API: `Loads`, `LoadsStrict`, `LoadsInto`,
`Dumps`, `DumpsForceStrings`, `EmitCanonical`, `FormatSource`,
`CanonicalFromSource`.

## API

- `ParseJSONLossless(b []byte) (Tree, error)` — exactly one complete
  JSON document; duplicate decoded keys at every depth, invalid UTF-8,
  lone surrogate escapes and trailing data are refused; numbers stay
  `json.Number` (source lexeme, no float64 round trip).
- `EncodeKtav(t Tree) ([]byte, error)` — canonical Ktav via
  `pure.EmitCanonical`; typed sentinel refusals (scalar root, empty
  key, CR byte, int64 overflow, binary64 overflow, value-changing
  decimals, multiline whitespace collisions). Never silently converts
  numbers to strings.
- `DecodeKtav(b []byte) (Tree, error)` — strict parse
  (`pure.LoadsStrict`), mapped back onto `json.Number` so `Equivalent` can compare.
- `ToJSON(t Tree) ([]byte, error)` — deterministic JSON: sorted keys, no
  HTML escaping, one trailing newline.
- `Equivalent(a, b Tree) error` — exact semantic equality; numbers by
  exact mathematical value via `math/big`; the error names the JSON
  path of the first difference and never prints values.
- `ConvertJSONToKtav(jsonBytes []byte) ([]byte, error)` — parse → encode
  → strict decode → equivalent; any failure returns an error and no bytes.

## Corpus (vendoring + refresh)

`testdata/spec-0.8.1` is vendored from the Ktav spec conformance corpus:
repo `github.com/ktav-lang/spec`, tag `v0.8.1`, path `versions/0.8/tests`.
`LICENSE-MIT` and `LICENSE-APACHE` are kept byte-for-byte from the corpus
root; fixture files must never be modified by hand. Categories: `valid`,
`invalid`, `unrepresentable`, `parseable-unrepresentable`, `strict-lossy`.

To refresh, re-copy from the tag:

```sh
git clone --branch v0.8.1 https://github.com/ktav-lang/spec
rm -rf internal/ktavcodec/testdata/spec-0.8.1
cp -R spec/versions/0.8/tests internal/ktavcodec/testdata/spec-0.8.1
```

Then keep `manifest.json` (exact per-category counts; the conformance runner
fails on drift, unknown/empty category, or skipped fixture) and the pinned
`expectedCounts` in `conformance_test.go` in sync with the new corpus.
