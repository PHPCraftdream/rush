# Custom roles

A custom role names a configured provider/model profile and an optional tool-name restriction. The built-in `smart`, `fast`, `worker` and `reviewer` slots remain separate and cannot be redefined through this registry. A custom role name is a case-sensitive configuration key; it is not a new built-in policy slot.

## Create and inspect roles

```sh
rush roles set researcher provider/model@high --tools '["view","grep","mcp_*"]'
rush roles set composer provider/model --tools '[]'
rush roles set general provider/model
rush roles list --json
rush roles show researcher --scope global --json
```

Provider/model specifications use the existing model resolver. Model atoms are also accepted. Effort is optional and validated against the known model's advertised capabilities where available. Raw model IDs outside the cached catalog retain the existing unverified passthrough; editing a role does not make a paid model request to verify them.

The provider must already exist in the effective configuration. Names may contain Unicode and internal spaces; quote such names in the shell. Empty names, leading/trailing whitespace, control characters, path separators and built-in names (including their case variants) are rejected.

## Select a custom role for execution

A custom role affects an execution only when its exact configured name is requested. For a top-level run, use `rush run --role <name>`; an SDK caller supplies the requested name through `RunOverrides.RequestedRole`; delegated `agent` input uses its `role` field. Batch and API task/run adapters carry the same exact name. For example:

```sh
rush run --role researcher "Summarize the project"
```

```go
req := sdk.RunRequest{
    Prompt: "Summarize the project",
    Overrides: sdk.RunOverrides{RequestedRole: "researcher"},
}
result, err := client.Run(ctx, req)
```

The name must resolve to a configured role. Invalid, absent-from-configuration, or mismatched explicit selections are errors; they must not silently fall back to `smart` or another model. The selected custom role is a model choice, separate from `ModelRole`, which remains the built-in policy role used by existing behaviors such as worker preference and reviewer policy. Selecting a custom name does not itself grant tools or permissions.

A delegated `agent.input.role` is likewise a requested model profile, not a request to replace the caller's policy. If omitted, existing automatic legacy model selection remains available. If explicitly set, it must be a valid built-in or configured custom role. On resume, an already-bound role is authoritative: omitting the role preserves it, while requesting a different role is rejected rather than rebinding the session.

## Runtime snapshot, inheritance, and continuation

At admission, the exact custom-role definition is resolved into a role contract containing the role name, full model-profile snapshot, tools-field presence/value, and inherited tool-cap groups. The contract is associated with the logical call and with any child session that is bound to that role. This keeps concurrent calls and resumed work from borrowing another call's selection.

A role's own tool list and every inherited cap group are intersected: a tool must be allowed by all present groups. A child role can narrow its parent's authority, never widen it. A missing `tools` field means that role adds no restriction of its own; it does not erase an inherited ceiling. The role contract's model snapshot, tool presence, and inherited groups are fixed for that logical call/child binding. Editing a role applies to a later admission, not to an already-bound call; if the role definition is removed after admission, bound work continues with its admitted model snapshot (provided its provider remains available). This is not a promise that an already-running provider request changes model mid-request. Built-in role model refresh behavior is unchanged.

The role contract is durable in the additive `session_role_contracts` table; root logical-call bindings are stored separately in `session_root_role_contracts` (migration `20261110000001_session_role_contracts`). A binding is immutable for its session or root logical call: a conflicting replacement or corrupt snapshot fails closed. Durable call options use schema version 3 to carry the role contract. Legacy v1 and v2 call-option rows remain readable only without a role contract; v1 is also rejected if it carries the v2-only `WebTurnOptIn` marker. Legacy sessions without a custom-role contract retain their pre-existing behavior; the new cap is not retroactively inferred for those rows.

In a credentialed/tenant call, the requested custom name must have an exact role slot in the tenant's credential model map. The operator's configured role is not an implicit substitute for a tenant slot. Configured-role fallback is available only through the trusted, existing `AllowConfiguredRoleFallback` exception; an untrusted request cannot enable it. An operator-side role cap, when present in the inherited contract, remains immutable and applies in addition to the tenant's selected model.

## Tools: omitted is different from empty

```json
{
  "roles": {
    "researcher": {
      "provider": "provider",
      "model": "model",
      "reasoning_effort": "high",
      "tools": ["view", "grep", "mcp_*"]
    },
    "composer": {
      "provider": "provider",
      "model": "model",
      "tools": []
    },
    "general": {
      "provider": "provider",
      "model": "model"
    }
  }
}
```

- No `tools` field: no additional restriction from this role.
- `tools: []`: no tools are allowed by this role, including `ask_question`, `await_tasks`, and batch-control tools. No runtime repair or control-tool exception makes an empty list nonempty.
- A nonempty array: a tool is allowed by this role's filter when any entry matches its full canonical name.
- `tools: null`: invalid. It is never interpreted as unlimited access.

Matching is case-sensitive and against the whole canonical tool name. `*` matches any sequence, `?` matches one Unicode code point, and every other character is literal. For example, `mcp_*` can match any name with that prefix; it does not mean “read-only MCP tools.” The filter is a tool-name policy, not a sandbox: allowing `bash` does not constrain shell command arguments or filesystem access.

Role filters only narrow the tools otherwise available. They do not bypass permissions, allowed-tool baselines, depth/folder constraints, or any inherited role ceiling. A denied tool call is not repaired into a different tool or dispatched under a broader driver.

The canonical registry key is lowercase `roles`; case aliases such as `Roles` are rejected rather than bypassing whole-definition merging or integrity protection.

## Update without accidentally widening tools

```sh
# Keep the existing scoped tools field while changing the model/effort.
rush roles set researcher provider/another-model@high

# Explicitly prohibit every tool.
rush roles set researcher provider/model --tools '[]'

# Explicitly remove the tools field.
rush roles set researcher provider/model --inherit-tools
```

`--tools` takes a JSON array, not a comma-split shell expression. `--tools` and `--inherit-tools` are mutually exclusive. Model-only edits do not silently replace an existing tools list with unrestricted access.

## Scopes and deletion

Writes default to the global scope. Use `--scope local` (or `--local`) for the managed workspace scope, and `--scope global` (or `--global`) for global configuration. Unknown, explicitly empty or conflicting scopes are rejected.

Each role definition overrides lower-priority definitions **as a whole**. Provider/model/effort and tools from different definitions are not combined, and tool arrays are not appended. A new definition without `tools` has no role restriction; when intentionally creating a local override, specify `--tools` if you want to retain a lower definition's ceiling.

```sh
rush roles set researcher provider/model --scope local --tools '["view","grep"]'
rush roles remove researcher --scope local
```

Removing a local definition may reveal the global definition. It is not a global ban on that name. `list` and `show` display effective definitions and source labels; an explicit scope reads only that scope's document. Removing a role prevents new admissions by that exact name, but does not discard a previously admitted role contract from already-bound work.

## Web settings

Open **Default models → Custom roles**. Choose System or Folder, then create a role or edit an existing definition. Provider/model suggestions use the existing catalog; raw model IDs remain editable. The tools selector has three explicit modes: no additional restriction, no tools, or a JSON pattern list.

Model-only edits omit unchanged tools so they do not overwrite another client's newer tool restriction. Server-side validation remains mandatory. Errors keep the form draft; an ambiguous transport failure does not trigger an automatic mutation retry. Refresh before deciding whether to retry.

CLI and external API writes retain settings-lock protection. Only the existing authenticated Web-UI transport has its scoped settings-write authority; adding `trustedSettingsWrite` to JSON, using a loopback address or holding an admin API token does not confer that authority. Role writes use the existing atomic/CAS writer, signature and audit pipeline. Integrity signatures cover both the model profile and tools presence; JSON/Ktav migration does not bless a pre-existing invalid signature.
