# Batch CLI spec formats

`rush batch run` and `rush batch add` accept either JSON or Ktav specifications. A file ending in `.ktav` is parsed as Ktav; other filenames are JSON. With `-f -` (the default), the entire stdin payload is parsed as JSON only when it is one complete valid JSON document; otherwise it is parsed as Ktav. Invalid input fails in the selected parser; there is no fallback after JSON was selected.

Example JSON:

```json
{"mode":"parallel","items":[{"tool":"run_command","input":{"program":"echo","args":["hello"]}}]}
```

Equivalent Ktav:

```ktav
mode: parallel
items: [
    {
        tool: run_command
        input: {
            program: echo
            args: [
                hello
            ]
        }
    }
]
```

Use `rush batch run -f jobs.ktav` to run a root specification. `rush batch add ROOT -f items.ktav` reads a batch containing the items to append (the root-level batch options are ignored by add; its `items` are appended).
