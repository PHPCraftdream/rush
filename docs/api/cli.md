# Remote API CLI

`rush api` talks to a Rush server started with `rush serve`. It never opens the local database, never creates `.rush/` and has no `--token` flag: credentials come from a profile (`--token-env`, `--token-file`, `--token-stdin`). Any argument or flag value that looks like a token is refused with exit 2 and the advice to rotate it; the value is never echoed.

Quick start:

```sh
rush api remote add staging --url https://rush.example --token-stdin --default
rush api sessions create --title review --json
rush api run SESSION "Review the change" --stream
rush api events SESSION --after 17 --follow --json
rush api permissions allow PERMISSION_ID
```

## Global flags

These apply to every `rush api` command.

- `--remote`: profile name; falls back to `RUSH_REMOTE`, then the default profile. A name that looks like a token is refused.
- `--ca`: CA certificate file for this call (overrides the profile).
- `--pin`: TLS pin for this call.
- `--client-cert`: client certificate for this call.
- `--client-key`: client key for this call.
- `--allow-cleartext`: allow plain HTTP to a non-loopback host for this call.
- `--insecure-skip-verify`: skip certificate verification; loopback URLs only, warns on stderr every time, never stored.
- `--json`: machine output: one object per line for lists and streams, one object otherwise.
- `--quiet`: suppress non-error output.
- `--timeout`: bound the WHOLE command (default 60s for simple requests, none for run, events and wait); exit 5 on expiry.

## Exit codes

- `0`: success.
- `1`: failed or cancelled run, network, TLS or server error.
- `2`: usage or configuration error (including no remote selected and token-shaped input).
- `3`: authentication or scope refused.
- `4`: not found.
- `5`: timeout.
- `6`: run paused for a human decision.
- `130`: interrupted (Ctrl+C). For `run` the remote run is cancelled best effort first.

## Commands

### rush api remote

Manage global remote profiles (stored in the user-global config, never in a project).

### rush api remote add

Add a profile. Exactly one credential source is required.

- `--url`: API base URL (`https://`, or `http://` to a loopback host).
- `--token-env`: name of the environment variable holding the token (read at every use).
- `--token-file`: absolute or home-relative token file (permissions are checked; on Windows it must sit under the user profile).
- `--token-stdin`: read the token from a hidden prompt or one line of stdin; it is validated against the token format and stored in a protected file.
- `--ca`: CA certificate file to trust.
- `--pin`: TLS pin: `sha256//BASE64` (public key) or `cert-sha256:HEX`.
- `--client-cert`: client certificate for mTLS.
- `--client-key`: client key for mTLS.
- `--allow-cleartext`: allow plain HTTP to a non-loopback host.
- `--default`: make this the default profile.
- `--replace`: replace an existing profile.

### rush api remote list

List profiles (`--json`: one object per line). The credential column shows the source, never the token.

### rush api remote show

Show one profile; the credential source only.

### rush api remote rm

Remove a profile and its managed token file.

### rush api remote test

`GET /health` and `GET /version` through the full client; prints the server version, the TLS mode and whether a pin is configured.

### rush api sessions

Session commands.

### rush api sessions list

List sessions (table, or `--json` one object per line).

- `--after`: return items after this cursor.
- `--limit`: page size (1-10000).
- `--all`: follow pagination up to the hard cap of 10000 items.

### rush api sessions get

Show one session.

### rush api sessions create

Create a session.

- `--title`: session title.

### rush api sessions fork

Fork a session.

- `--title`: title of the fork.

### rush api sessions delete

Delete a session (`--quiet` prints nothing).

### rush api messages

List the messages of a session.

- `--after`: return items after this cursor.
- `--limit`: page size (1-10000).
- `--all`: follow pagination up to the hard cap of 10000 items.

### rush api models

List the models the remote offers.

### rush api openapi

Print the raw OpenAPI document to stdout.

### rush api raw

Send a request to a path relative to `/api/v1`; the response body goes to stdout and `HTTP <status>` to stderr.

- `--data`: request body file, or `-` for stdin.

### rush api run

Create a run in a session. By default it waits and prints the final answer text. Exit 6 when the run pauses for a human: stderr names the pending permission and the exact `rush api permissions allow ID` command.

- `--role`: role for the run.
- `--model`: model override for the run.
- `--no-wait`: print the run id and return.
- `--stream`: print events as they arrive; ends on the run's terminal state with the same exit codes.

### rush api runs

Run commands.

### rush api runs get

Show a run.

### rush api runs wait

Wait for a run to finish or pause (bounded by the global `--timeout`).

### rush api runs cancel

Request cancellation of a run.

### rush api cancel

Short form of `runs cancel RUN`.

### rush api events

Replay and follow the session event stream (SSE with `Last-Event-ID` resume and reconnect). Without `--follow` it exits after the replay has been idle for `--idle`.

- `--after`: resume after this numeric event id.
- `--follow`: keep streaming until Ctrl+C (exit 130).
- `--idle`: idle window that ends a non-follow replay (default 1s). The server marks no end of replay, so a very slow link may be cut short.

### rush api inject

Add text to the active run of a session.

### rush api permissions

Permission requests of runs.

### rush api permissions list

List pending requests.

- `--session`: filter by session id.

### rush api permissions allow

Allow one request once (`allow_session` is not offered).

### rush api permissions deny

Deny one request.
