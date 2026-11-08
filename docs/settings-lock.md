# Settings lock and the trusted-UI write exception

## 1. Overview

The settings lock lets a user forbid changes to settings while work is in
progress. Locking stores a PBKDF2-SHA256 password hash in the `sync_rev` key of
the global or workspace settings document
(`internal/config/settings_lock.go`: `HashPassword`, `VerifyPassword`,
`LockSettings`, `UnlockSettings`). While a lock hash is present, the write
guard vetoes protected writes with `ErrSettingsLocked` (or `ErrWrongPassword`
when a process password is configured and does not match). OAuth token refresh
writes and `recent_models.*` writes persist through their own pre-existing
exemption mechanisms and are unaffected.

Task #57 adds one narrow exception: a write that arrives through the
web server's UI-authenticated listener can present a per-call
`SettingsAuthorityTrustedUI` authority (`internal/config/settings_authority.go`),
which lifts ONLY the settings-lock veto for that single write call. Everything
else about the lock — and everything else about the write pipeline — is
unchanged.

## 2. The write pipeline

`ConfigStore.guardSettingsWriteForAuthority`
(`internal/config/settings_lock.go`) checks, in order:

1. **Reserved keys (always).** Any write touching `sync_rev` or `cache_rev`
   (`reservedSettingsKeys`) is refused for every authority.
2. **`recent_models.` exemption (always).** If every key in the write starts
   with `recent_models.`, the write proceeds for every authority.
3. **Settings-lock veto (guarded only).** Otherwise the write consults the
   current lock sources (`checkExternalSettingsSources`, which re-reads the
   global/workspace lock hashes under the write lock). This is the ONLY check
   that `SettingsAuthorityTrustedUI` skips (`liftsSettingsLock`).

What is NEVER lifted by any authority, including the trusted-UI one:

- Reserved `sync_rev` / `cache_rev` writes.
- Model-integrity `cache_rev` signing (`applyModelsIntegrity`).
- CAS / atomic commit of the settings document.
- Scope and path ownership rules (which file a write targets and who owns it).
- Interprocess locks around settings files.
- Atomic commit / reload semantics after a successful write.
- Failure outcomes: a refused write still returns `ErrSettingsLocked` and still
  emits an audit event with a refused outcome.

## 3. Where the trusted-UI authority comes from

The authority is derived from the transport, and from nothing else:

- ONLY the standard UI listener (the `uiAuth` server) grants it. On that
  listener every connection passes `Auth.Middleware` — verifying the session
  cookie or Bearer token — before the WebSocket upgrade is accepted.
- `Server.handleWS` (`internal/server/server.go`) marks only such connections
  with `Client.trustedSettingsWrite`; the accessor
  `Client.settingsWriteAuthority()` returns `SettingsAuthorityGuarded` for
  every other client: policy serve / API listeners, synthetic clients, CLI and
  agent-facing writes, and unauthenticated connections.
- No JSON field, HTTP header, loopback source address, missing Principal, or
  the WebSocket transport itself can produce the trusted authority. The
  config-writing WS handlers (`internal/server/handlers_config.go`,
  `handlers_models.go`, `handlers_providers.go`, `handlers_mcp.go`) simply
  thread `c.settingsWriteAuthority()` into the store calls, so a guarded
  client gets today's behavior with zero policy changes.
- Holding the full UI token or cookie means operator authority over that
  listener; it is not proof of a physical person.

## 4. MCP mutations

MCP configuration mutations must go through the current Owner
(`internal/agent/tools/mcp`). The TrustedUI variants —
`Owner.DisableServerTrustedUI`, `EnableServerTrustedUI`,
`AddServerTrustedUI`, `ReplaceServerTrustedUI`, `RemoveServerTrustedUI`
(`internal/agent/tools/mcp/trusted_ui.go`) — reuse the identical lifecycle
machinery as their guarded counterparts and honor the same invariants:

- owner identity and `ConfigStore` binding,
- leases before lifecycle locks,
- admission checks and epoch fencing,
- pending-add handling,
- uncertainty fencing,
- token-conditional enable rollback,
- persist-before-invalidate ordering.

The trusted authority reaches exactly one place: the durable persistence
seam — the `Persist*WithAuthority` methods on `ConfigStore`
(`internal/config/store_mcp.go`). All lifecycle decisions are made before it,
unchanged.

## 5. Audit

`config.SettingsEvent` gained a `Source` field (`internal/config/settings_lock.go`).
It is either `""` (ordinary writes) or the fixed token `"ui"` (trusted-UI
writes, via `settingsAuditSource` in `internal/config/settings_authority.go`).
It never carries secrets, credentials, or user identifiers. The CLI audit sink
(registered with `config.SetSettingsAuditSink` in `internal/cmd/root.go`,
function `setSettingsAudit`) records the `source` label alongside `scope` and
`outcome`.

## 6. Guarantees against bypass windows

- The authority is a **per-call parameter** on the `*WithAuthority` write
  methods (`internal/config/store_write.go`, `store_options.go`,
  `store_models.go`, `store_oauth.go`, `store_mcp.go`). There is no
  process-wide toggle, no context key, and no temporary unlock mode to
  forget to re-arm.
- The zero value of `SettingsWriteAuthority` is `SettingsAuthorityGuarded`,
  so every existing caller inherits the old behavior.
- A concurrent CLI or agent write keeps hitting the same veto, because each
  write re-reads the lock sources (`checkExternalSettingsSources`) under its
  own write lock at commit time. There is no cached "unlocked" decision that
  another process's write could slip through.

## 7. Testing map

- `internal/config/settings_authority_test.go` — pins the authority type
  itself: zero value is guarded, only trusted-UI lifts the settings-lock
  veto, reserved keys and recent-models rules apply to every authority, and
  the audit source mapping (`""` / `"ui"`).
- `internal/server/handlers_config_lock_trusted_test.go` — pins that the
  UI-authenticated listener path actually carries the trusted authority into
  config writes under an active lock, and that non-UI transports do not.
- `internal/agent/tools/mcp/trusted_ui_test.go` — pins the TrustedUI Owner
  variants: lifecycle invariants hold and only the durable persistence call
  carries the trusted authority.
- `internal/server/handlers_config_lock_test.go` (pre-existing) — pins the
  guarded path: writes under lock are refused exactly as before.
- `internal/config/settings_lock_exemptions_test.go` (pre-existing) — pins
  the `recent_models.*` and OAuth refresh exemptions that stay.

## 8. Out of scope / unchanged

- The CLI lock/unlock commands and password verification (`LockSettings`,
  `UnlockSettings`, `VerifyPassword`, `CheckPasswordAgainstDisk`,
  `CheckProcessPassword`) are unchanged.
- The web UI cannot unlock settings without the password; the trusted
  authority lifts only the lock veto for individual writes, it is not an
  unlock.
- Weekly peak-hours scheduling is explicitly a separate task and is not part
  of this change.
