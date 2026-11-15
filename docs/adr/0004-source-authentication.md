# 0004: Sources authenticate with a token, a signature, or both

Status: accepted, November 2026

## Decision

A source needs `token`, `signature`, or both (then both must hold). A source
with neither is rejected by the config check and, as a second guard, by the
HTTP adapter.

`signature` is an HMAC-SHA256 of the raw body in a header, hex encoded, with
an optional prefix. Presets cover GitHub (`X-Hub-Signature-256`, `sha256=`),
Gitea and Forgejo. The secret never travels with the request, so a leaked
access log does not leak it. `token_header` reads the token from a fixed
header for senders like GitLab.

Order of checks: token, then signature (the body is read for it, at most
1 MiB), then the rate limit, then decoding. Requests that fail
authentication never reach the audit log, so nobody can flood it.

## Consequences

- Replay protection (timestamps in the signature, as Slack and Stripe do) is
  not there yet. The dedupe window catches exact replays within its time.
- mTLS is on the roadmap and fits in front of these checks.
