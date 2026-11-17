# Changelog

## [0.1.1] - 2026-11-17

### Added

- Template text helpers `firstLine`, `lines`, `trim`, `trunc`, `replace` and `regexFind`, for messages whose useful part sits inside a longer text, such as the release name on the first line of a cleanuparr notification.

### Changed

- Digests list identical messages once, with `×N` at the end of their first line, instead of repeating them.

## [0.1.0] - 2026-11-15

First release.

### Added

- Webhook ingest at `POST /in/{source}` with per-source tokens (Bearer, Basic auth password, `?token=` or a fixed `token_header`) and HMAC-SHA256 signatures with presets for GitHub, Gitea and Forgejo.
- Source kinds `alertmanager` (with optional `split_alerts`), `generic` (JSON or plain text) and `cloudevents` (structured, batch and binary mode).
- Ordered routing rules: source globs, status, labels and JSON fields as anchored regexes with `!` negation, optional expressions, `drop`, `continue`, several destinations per rule, `priority` (low, normal, high, urgent).
- Pluggable expression engines behind `domain.Engine` with a contract suite. CEL is built in and the default, `engine:` selects one globally or per route.
- Go templates rendered as Markdown, built-in templates per source kind.
- Targets `matrix` (end-to-end encrypted bots, cross-signing, one device per node), `email` (SMTP with STARTTLS or implicit TLS), `ntfy` and `webhook`, all with multiple accounts per target.
- Fallback destinations: when delivery finally fails, the destination's fallbacks get the message. Shown in the audit log as `delivered via fallback`.
- Durable spool (`delivery.spool`, SQLite): accepted deliveries survive restarts and crashes and are resumed on start.
- Heartbeats with alert and recovery messages.
- Delivery in `sync` or `async` mode with per-account queues, retries with backoff, dedupe window and a drained shutdown.
- Optional high availability with static peers, leader election, shared heartbeats and dedupe. `-tags noha` builds without it.
- Dataflow UI with live counters and a payload tracer.
- Configuration in three layers: YAML file and fragments, then `STELLWERK_*` environment variables (with `_FILE`), then secret files such as mounted Kubernetes Secrets. `-print-config` shows the merged result with secrets redacted.
- Audit log of every received message: payload, labels, each rule's verdict and every send attempt with its error. In the UI with filters and detail view, as JSON API, and optionally as a rotated JSON lines file or log lines.
- Prometheus metrics on their own listener.
- Rate limits per source with `429` and `Retry-After`.
- End-to-end test against a real Synapse, run in CI.
- JSON logging, `/healthz`, `/readyz`, Kubernetes manifests, Docker image.
- `ROADMAP.md` with the backlog towards high volume and critical use, and architecture decision records in `docs/adr`.
- Silences: mute by matchers for a time, from the UI, the API or the chat. Stored in the spool, shown in the audit log and the simulator, also for heartbeat alarms.
- Chat commands for Matrix accounts with `commands: true`: `!stellwerk mute`, `unmute`, `silences`, `status`, `canary`.
- Digests: `digest: 1h` on a route sends one summary per destination and window.
- Ping URLs for heartbeats with a `token`: `/ping/<name>` and `/ping/<name>/fail`, like healthchecks.io.
- Canary: a scheduled test message through the whole chain, optionally read back and decrypted by a second Matrix account.
- `self_ping`: stellwerk reports to an outside service that it is alive, or what keeps it from delivering.
- Matrix health checks: rooms the bot is not in, unencrypted rooms and a device without cross-signing are reported in the UI, metrics and the self ping.
- Smarter retries: permanent errors are not retried, `Retry-After` and Matrix rate limits are honoured, jitter, a circuit breaker per account, a note on late messages and an optional maximum age.
- Reload without restart: SIGHUP, the UI button or `-watch` for changed files. Changes that need a restart are refused whole.
- Send again from the audit log, all deliveries or only the failed ones.
- ntfy and Gotify compatible endpoints (`/ntfy`, `/gotify`) and source kinds, so apps that speak them work by changing the server URL.
- Rule tests in the config (`tests:`), run with `stellwerk -test`.
- `stellwerk init` sets up a Matrix bot, an encrypted room and a working config. `stellwerk send` posts from scripts.
- JSON Schema for the config (`stellwerk -schema`, `docs/stellwerk.schema.json`) for editor completion.
- Recipes for Alertmanager, Grafana, Uptime Kuma, the *arr apps, Proxmox, TrueNAS, Home Assistant, Watchtower, Diun, Git platforms and cron jobs.
- Releases with Linux binaries for amd64 and arm64, a cosign-signed image with SBOM and provenance, signed checksums, build provenance attestations for the binaries and the chart, and a Helm chart in ghcr.io.
- `make check` as the definition of green, run by CI and the pre-push hook: golangci-lint, tests with the race detector, with and without HA, a SIGKILL and restart test, a coverage floor, govulncheck, a license allowlist, gitleaks, schema and example config checks and fleetlint. Also in CI: fuzzing of decoders and parsers, CodeQL, Helm lint and schema validation, weekly mutation testing of the domain core, Renovate. Workflows are pinned to commit SHAs and checked with actionlint and zizmor.
- License: Apache 2.0.

### Fixed

- Audit entries created in the same millisecond by different parts (an event and a heartbeat alarm) could get the same ID.
