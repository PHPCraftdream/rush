# TLS listeners

> **Status:** Every statement on this page describes the design only; it is not yet implemented. (lane 9, not yet merged)

The design proposes HTTPS and WSS listeners configured with `--tls-cert` and `--tls-key`, or `--tls-self-signed`; all statements in this paragraph are lane 9, not yet merged. (lane 9, not yet merged)

The design proposes a `server.listeners[]` TLS configuration with `tls.cert` and `tls.key`; it sets TLS 1.3 minimum by default, with optional `tls.min_version: "1.2"` and the specified restricted cipher suites. These remain proposal-only, not current listener behavior. (lane 9, not yet merged)

Planned certificate reload validates the certificate/key pair and certificate properties, compares content hashes, atomically retains the last good certificate on invalid updates, and retries; reload and expiry warnings are design requirements only. (lane 9, not yet merged)

The planned self-signed mode uses an ECDSA P-256 certificate and persists/reuses its key; SANs include loopback names/addresses and may be extended with `--tls-san`. (lane 9, not yet merged)

The design's `--client-ca` option enables required, verified client certificates; bearer tokens remain responsible for API identity and scopes. (lane 9, not yet merged)

See the [design's TLS section](../plans/2026-11-01-external-api-design.md#9-tls-c3c4) for the full proposal; that document is design, not evidence of shipped functionality. (lane 9, not yet merged)

