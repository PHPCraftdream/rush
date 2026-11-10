# Custom role registry

A custom role names a provider/model profile and an optional tool-name restriction.
The built-in `smart`, `fast`, `worker` and `reviewer` slots remain separate and cannot be redefined through this registry.

> Delivery boundary: this change adds the registry, CLI and Web editing. Selecting custom roles during agent execution, inherited tool ceilings and durable continuation enforcement are delivered by the dependent runtime integration. A stored definition alone does not change an existing agent's tools or the built-in slots.

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
- `tools: []`: no tools.
- A nonempty array: any matching name is permitted by this role's filter.
- `tools: null`: invalid. It is never interpreted as unlimited access.

The matching contract uses full canonical tool names, case-sensitive, with only `*` (any sequence) and `?` (one character) as wildcards. All other characters are literal. Names and masks must be nonempty; masks do not have to match a currently connected MCP tool when saved. `mcp_*` includes every matching MCP tool, not only read operations.

A role filter does not grant tools beyond existing permissions, allowed-tool baselines, depth/folder constraints or inherited ceilings. Allowing `bash` is not a filesystem sandbox and does not restrict shell command arguments.

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

Removing a local definition may reveal the global definition. It is not a global ban on that name. `list` and `show` display effective definitions and source labels; an explicit scope reads only that scope's document.

## Web settings

Open **Default models → Custom roles**. Choose System or Folder, then create a role or edit an existing definition. Provider/model suggestions use the existing catalog; raw model IDs remain editable. The tools selector has three explicit modes: no additional restriction, no tools, or a JSON pattern list.

Model-only edits omit unchanged tools so they do not overwrite another client's newer tool restriction. Server-side validation remains mandatory. Errors keep the form draft; an ambiguous transport failure does not trigger an automatic mutation retry. Refresh before deciding whether to retry.

CLI and external API writes retain settings-lock protection. Only the existing authenticated Web-UI transport has its scoped settings-write authority; adding `trustedSettingsWrite` to JSON, using a loopback address or holding an admin API token does not confer that authority. Role writes use the existing atomic/CAS writer, signature and audit pipeline. Integrity signatures cover both the model profile and tools presence; JSON/Ktav migration does not bless a pre-existing invalid signature.
