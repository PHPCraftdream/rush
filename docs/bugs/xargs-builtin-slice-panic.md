# Bug — built-in `xargs` panics (`slice bounds out of range [:-1]`) and kills the whole `rush run`

**Status:** open. Found 2026-10-18.
**Version:** `rush version 0.2.0-alpha.16 (c8969428, built 2026-10-11_20:53:40)`, Windows 10 (Git Bash host).
**Observed in:** `D:\dev\rust\sefer-alloc` (cwd `worktrees\perf-cand`), session `b4-r1-r2` (hash `48ace7f6`), `rush run --role smart --session b4-r1-r2 --timeout 180m --json`.

## Symptom

The agent issued an ordinary bash tool call; the in-process shell
interpreter (mvdan.cc/sh) dispatched `xargs` to the u-root built-in, which
panicked. The panic was not contained to the tool call: the entire
`rush run` process died with exit code **2**, wrote **nothing** to stdout
(`--json` result empty — the orchestrator gets no `review`/`final_text`),
and the session was left mid-turn. Only a goroutine dump on stderr remained.

The triggering tool call (from `rush sessions last b4-r1-r2`):

```
ls tests/*.rs | xargs -n1 basename | rg "loom" ; echo ==== ; sed -n '1,60p' tes…
```

## Stack (stderr)

```
panic: runtime error: slice bounds out of range [:-1] [recovered, repanicked]

goroutine 2701 [running]:
sync.(*WaitGroup).Go.func1.1()
	sync/waitgroup.go:251 +0x45
panic({0x7ff6182e0b40?, 0x2898a380c078?})
	runtime/panic.go:860 +0x13a
github.com/u-root/u-root/pkg/core/xargs.(*command).RunContext(0x2898a3a5e1e0, {...}, {0x2898a3322550, 0x2, 0x3})
	github.com/u-root/u-root@v0.16.0/pkg/core/xargs/xargs.go:127 +0xca5
...
mvdan.cc/sh/v3/interp.(*Runner).stmtSync(...)
	mvdan.cc/sh/v3@v3.13.1/interp/runner.go:343 +0x1d1
mvdan.cc/sh/v3/interp.(*Runner).stmt(...)
	mvdan.cc/sh/v3@v3.13.1/interp/runner.go:325 +0x2c9
mvdan.cc/sh/v3/interp.(*Runner).cmd.func1()
	mvdan.cc/sh/v3@v3.13.1/interp/runner.go:500 +0x35
sync.(*WaitGroup).Go.func1()
	sync/waitgroup.go:258 +0x4a
created by sync.(*WaitGroup).Go in goroutine 2700
	sync/waitgroup.go:238 +0x73
```

The pipeline stage runs in its own goroutine (`Runner.cmd.func1` under
`sync.WaitGroup.Go`), so the panic escapes any per-tool-call recover and
takes the process down (`[recovered, repanicked]`).

## Likely trigger

`xargs -n1 <cmd>` (the `-n` max-args form) with the u-root built-in;
`RunContext` is called with 2 args (`{..., 0x2, 0x3}` = `["-n1",
"basename"]`?) and slices `[:-1]` on an empty/short slice at `xargs.go:127`.
Plain `xargs <cmd>` without `-n` worked earlier in another session of the
same project (`... | xargs rustfmt --edition 2021 --check`, exit 0).
Not yet confirmed whether the input (`ls tests/*.rs` output, 427 lines,
Windows paths) or the `-n1` flag is the decisive part.

## Expected

1. `xargs -n1 basename` works (or fails with an error message) like GNU xargs.
2. A panic inside any built-in / pipeline goroutine is recovered and turned
   into a failed tool result (non-zero exit + message) for that one call; the
   agent run continues.
3. If the process must die, `--json` still emits a result object (status
   `crashed`, last error) so orchestrators can see what happened.

## Repro sketch

```
rush run --role smart --session xargs-repro --json <<'EOF'
Run exactly this bash command and report its output:
printf 'a/x.rs\nb/y.rs\n' | xargs -n1 basename
EOF
```

## Workaround

Tell agents not to use `xargs` (use `for f in ...; do ...; done`). The
relaunch of session `b4-r1-r2` with that instruction proceeded normally.

## Addendum 2026-10-19 — delegated workers re-trigger it

Session `b4-hot` (`rush run --role smart`, a Worker model configured, so the
smart agent delegates): the parent prompt forbade `xargs`, but a delegated
worker issued `git diff --name-only -- '*.rs' | xargs rustfmt --check ...` /
`grep -c ... | xargs echo $n` and the same u-root panic
(`xargs.go:127 slice bounds out of range [:-1]`) killed the whole parent
`rush run` (exit 2, empty `--json` stdout) while the parent was in
`await_tasks`. So:

- the crash is not limited to `-n1`: plain `... | xargs <cmd>` panics too
  (with 2 args `{..., 0x2, 0x3}` again at `RunContext`);
- a worker's panic is not contained to the worker — the orchestrating parent
  process dies with it;
- parent-prompt rules are not propagated to delegated workers, so a
  prompt-level workaround is incomplete. Expected: contain builtin panics per
  tool call, and/or let the caller pass command bans (e.g. a
  `--deny-command xargs` flag) that apply to the whole delegation tree.
