# Batch CLI spec formats

`rush batch run` accepts JSON or Ktav specifications. The parser used by `rush batch add` accepts the same encoding for its appended items, but add is owner-process-only: a separate `rush batch add` CLI process refuses to add to an existing runner. A file ending in `.ktav` is parsed as Ktav; other filenames are JSON. With `-f -` (the default), the entire stdin payload is parsed as JSON only when it is one complete valid JSON document; otherwise it is parsed as Ktav. Invalid input fails in the selected parser; there is no fallback after JSON was selected.

Example JSON:

```json
{"mode":"parallel","items":[{"tool":"run_command","input":{"program":"git","args":["status","--short"]}}]}
```

Equivalent Ktav:

```ktav
mode: parallel
items: [
    {
        tool: run_command
        input: {
            program: git
            args: [
                status
                --short
            ]
        }
    }
]
```

Use `rush batch run -f jobs.ktav` to run a root specification. A spec for `rush batch add` has the same encoding but can only be applied in the owning process; a separate CLI process cannot add to a live runner.
