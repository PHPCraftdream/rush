"""Go import parsing/rewriting, non-Go token scanning, declaration extraction."""

import re
import string

try:
    from . import gitops
except ImportError:
    import gitops

_IMPORT_BLOCK_RE = re.compile(r"\bimport\s*\(([^)]*)\)", re.S)
_IMPORT_SINGLE_RE = re.compile(r'\bimport\s+(?:([A-Za-z_][A-Za-z0-9_]*|_|.)\s+)?"((?:[^"\\\n]|\\.)*)"')
_QUOTED_RE = re.compile(r'"((?:[^"\\\n]|\\.)*)"')


def read_module(repo):
    path = repo.root / "go.mod"
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line.startswith("module ") or line.startswith("module\t"):
                return line.split(None, 1)[1].strip()
    raise RuntimeError("no module directive found in go.mod")


def resolve_import(path, module, moves):
    """Return rewritten module-internal import path, or None if unchanged/not internal."""
    if path == module:
        tail = "."
    elif path.startswith(module + "/"):
        tail = path[len(module) + 1:]
    else:
        return None
    new_tail = moves.resolve(tail)
    if new_tail is None:
        return None
    if new_tail == tail:
        return None
    return module + "/" + new_tail


def _iter_quoted(span_text):
    for m in _QUOTED_RE.finditer(span_text):
        yield m.start(1), m.end(1), m.group(1)


def rewrite_go_source(text, module, moves):
    """Return (new_text, [(old_path, new_path), ...])."""
    spans = []
    changed = []
    offset = 0
    # build (abs_start, abs_end, path) for all quoted strings inside import decls
    for bm in _IMPORT_BLOCK_RE.finditer(text):
        base = bm.start(1)
        for s, e, p in _iter_quoted(bm.group(1)):
            new = resolve_import(p, module, moves)
            if new is not None:
                spans.append((base + s, base + e, new))
                changed.append((p, new))
    for sm in _IMPORT_SINGLE_RE.finditer(text):
        p = sm.group(2)
        new = resolve_import(p, module, moves)
        if new is not None:
            spans.append((sm.start(2), sm.end(2), new))
            changed.append((p, new))
    new_text = text
    for s, e, rep in sorted(spans, key=lambda t: t[0], reverse=True):
        new_text = new_text[:s] + rep + new_text[e:]
    return new_text, changed


_BLOCK_LINE_RE = re.compile(r'^\s*(?:([A-Za-z_][A-Za-z0-9_]*|_|.)\s+)?((?:"(?:[^"\\\n]|\\.)*"\s*)+)')


def scan_import_specs(text):
    """List of (alias_or_None, path) for all import specs."""
    specs = []
    for bm in _IMPORT_BLOCK_RE.finditer(text):
        for line in bm.group(1).splitlines():
            lm = _BLOCK_LINE_RE.match(line)
            if not lm:
                continue
            alias = lm.group(1)
            for q in _QUOTED_RE.finditer(lm.group(2)):
                specs.append((alias, q.group(1)))
    for sm in _IMPORT_SINGLE_RE.finditer(text):
        specs.append((sm.group(1), sm.group(2)))
    return specs


NONGO_TOKEN_CHARS = string.ascii_letters + string.digits + "_-./"
NONGO_NAME_CHARS = string.ascii_letters + string.digits + "_-"


def scan_nongo(text, moves, module):
    """List of (start, end, old_token, new_token) replacements."""
    olds = sorted(moves.mapping.keys(), key=len, reverse=True)
    if not olds:
        return []
    rx = re.compile("|".join(re.escape(o) for o in olds))
    spans = []
    covered = 0
    for m in rx.finditer(text):
        s = m.start()
        if s < covered:
            continue
        if s > 0 and text[s - 1] in NONGO_NAME_CHARS:
            continue
        e = m.end()
        while e < len(text) and text[e] in NONGO_TOKEN_CHARS:
            e += 1
        token = text[s:e]
        new = None
        if token.startswith(module + "/"):
            tail = token[len(module) + 1:]
            r = moves.resolve(tail)
            if r is not None:
                new = module + "/" + r
        else:
            new = moves.resolve(token)
        if new is not None and new != token:
            spans.append((s, e, new))
            covered = e
    # scan_nongo contract: (start, end, old, new)
    return [(s, e, text[s:e], n) for s, e, n in spans]


def rewrite_with_spans(text, spans):
    """Apply (start, end, replacement) spans in reverse order; return (text, n)."""
    out = text
    n = 0
    for s, e, rep in sorted(spans, key=lambda t: t[0], reverse=True):
        out = out[:s] + rep + out[e:]
        n += 1
    return out, n


_DECL_RE = re.compile(r"^(func|type|var|const) ")
_TEST_RE = re.compile(r"^func Test[A-Za-z0-9_]*[\[(]")
_PACKAGE_RE = re.compile(r"^package\s+([A-Za-z_][A-Za-z0-9_]*)")


def top_level_decls(text):
    out = []
    for line in text.splitlines():
        if _DECL_RE.match(line):
            out.append(line.rstrip())
    return out


def count_test_functions(text):
    return sum(1 for line in text.splitlines() if _TEST_RE.match(line))


def package_name(text):
    for line in text.splitlines():
        m = _PACKAGE_RE.match(line)
        if m:
            return m.group(1)
    return None
