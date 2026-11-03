# TLS listeners

`rush serve` can terminate TLS itself: `https://` for REST and `wss://` for WebSocket on the same listener. TLS is configured per listener; the `--tls-*` and `--client-ca` flags apply to the first listener.

## Enabling TLS

| Mode | How |
|---|---|
| Your certificate | `--tls-cert FILE --tls-key FILE`, or `tls.cert` / `tls.key` in `server.listeners[]` (both are required together) |
| Self-signed | `--tls-self-signed`, optionally with `--tls-san NAME` (repeatable) |
| Client certificates | `--client-ca FILE` or `tls.client_ca` (needs a server certificate or self-signed mode) |
| Minimum version | `tls.min_version`: `"1.3"` (default) or `"1.2"` |

A self-signed certificate cannot be combined with an explicit certificate or key. `rush serve --print-config` shows the effective TLS settings and where each came from.

## Protocol policy

- TLS 1.3 minimum by default. With `tls.min_version: "1.2"` only ECDHE with AES-GCM or ChaCha20-Poly1305 suites are offered.
- HTTP/1.1 only (ALPN `http/1.1`); HTTP/2 is not offered.
- Session tickets are disabled, so a session cannot be resumed from a ticket.

## Certificate handling

- The certificate and key are validated when the server starts: they must be a matching pair, the leaf must be usable for server authentication and carry a subject alternative name, and an RSA key must be at least 2048 bits. On Unix a key readable by group or others is refused; on Windows the file mode is not checked and a warning is logged.
- The files are re-read every minute. A change is detected by content hash, not modification time. A candidate that fails validation is logged and the last good certificate stays in service; the server keeps retrying. Replacing a certificate needs no restart.
- The server warns as the certificate approaches expiry and when it has expired.

## Self-signed certificates

The self-signed mode creates an ECDSA P-256 certificate valid for one year, with `localhost`, `127.0.0.1`, `::1`, the host name and the bind address, plus every `--tls-san`. The private key is stored in the data directory with mode `0600` and **reused** on later starts, so the public-key pin stays stable while the certificate itself is reissued. A listener on a wildcard address (`0.0.0.0`, `::`) requires at least one non-blank `--tls-san`. An existing certificate without its key is never replaced, and an expired one is refused.

When the server starts with a self-signed or explicit certificate it prints the pins on stderr:

```
rush serve: TLS public-key pin: <hex> (curl: sha256//<base64>); leaf fingerprint: cert-sha256:<hex>
```

Give the `sha256//…` value to `curl --pinnedpubkey` or store it in a `rush api remote` profile. The public-key pin survives certificate renewal; the leaf fingerprint does not.

## Client certificates (mTLS)

With `--client-ca`, the handshake requires a client certificate that chains to that CA; a client without one, or with one from another CA, never reaches HTTP. The certificate proves the connection only: **API identity and scopes still come from the bearer token**. The verified certificate's SHA-256 fingerprint is added to the audit line (`certificate_fingerprint`) next to the token id.

## Token revocation on open streams

An SSE stream holds its token's connection slot for its whole life. Every 15 seconds the server re-checks the token; a revoked or expired token gets a final `closed` event (`token_revoked`) and the stream is closed, freeing the slot.

## Client side

`rush api remote` profiles accept a CA file, a public-key pin and a client certificate and key; see [the CLI reference](cli.md). The design rationale is in [the design's TLS section](../plans/2026-11-01-external-api-design.md#9-tls-c3c4).
