# Roadmap

stellwerk is built for a homelab first: one node, a handful of sources, Matrix
as the main channel. This file is the backlog for the step after that, a
router for high volume and critical alerts. Nothing here is promised. Every
item lists the seam that already exists for it, so it can be built without
redesigning what is there.

Design decisions are recorded in [docs/adr](docs/adr). The rule for every
change: the domain stays pure, new technology enters as an adapter behind a
port, and every port has a contract suite that fakes and real adapters pass.

## Done for 0.1.0 (homelab grade)

| What | Where |
| --- | --- |
| Durable spool: accepted deliveries survive restarts and crashes | `ports.Outbox`, `adapters/out/spool` (SQLite) |
| Fallback destinations: mail or push when Matrix is down | `destinations.*.fallback`, `DeliverMessages` |
| Priorities low, normal, high, urgent | `domain.Message.Priority` |
| Pluggable expression engines, CEL built in | `domain.Engine`, `adapters/engine` |
| Targets email (SMTP) and ntfy | `adapters/out/target/{email,ntfy}` |
| Source kind cloudevents (structured, batch, binary) | `adapters/in/source/cloudevents.go` |
| HMAC signatures (GitHub, Gitea, Forgejo) and token headers | `adapters/in/http` |
| Silences from UI, API and chat, chat commands | `ports.ManageSilences`, `adapters/in/chatops` |
| Digests, ping URLs, canary with read-back, self ping | `DigestMessages`, `ports.ReceivePings`, `ports.RunCanary`, `ports.Receiver` |
| Retry classification, Retry-After, jitter, circuit breaker, message age | `ports.Permanent`, `ports.RetryAfter`, `DeliverMessages` |
| Reload, resend, rule tests, `init`, `send`, JSON Schema | `bootstrap`, `ports.ResendAudit`, `cmd/stellwerk` |
| ntfy and Gotify compatible endpoints | `adapters/in/http/compat.go`, source kinds `ntfy`, `gotify` |
| Signed releases, SBOM, Helm chart, fuzzing, kill test | `.github/workflows`, `deploy/helm`, `test/process` |

## Backlog

Ordered by what a critical setup needs first.

### 1. Replicated log instead of a local spool

Today: each node has its own SQLite spool. A node that dies with its disk
takes its unsent deliveries with it.

Next: NATS JetStream (Go, embeddable, Raft replicated) as the log between
ingest and delivery. Events get an offset, can be replayed, failed deliveries
go to a dead letter stream.

Seam: `ports.Outbox` (Put, Done, Pending) maps to publish, ack and the
unacked backlog. `Delivery.ID` is already the idempotency key. See ADR 0001.

### 1b. Shared silences, digests and ping state

Today: silences, open digests and the failed state of pings are per node.
A chat command reaches every node, a UI or API action only one.

Next: keep them in the coordinator's shared store (JetStream KV with item
2), so every node mutes the same and a digest survives a node.

Seam: `ports.SilenceStore` is already a port, a replicated adapter
replaces the SQLite one. Digests need a store port of their own.

### 2. Coordination by consensus

Today: the lowest live node ID leads, peers ping each other over HTTP. A
network split can make two leaders, so a heartbeat alarm can go out twice.

Next: leadership from a Kubernetes Lease or JetStream KV, dedupe keys and
heartbeat times in JetStream KV.

Seam: `ports.Coordinator`. `CoordinatorContract` already defines the
behaviour, a new adapter has to pass it.

### 3. Separate ingest from delivery

Thin stateless ingest pods (auth, validate, persist) and worker pods (route,
deliver), scaled on their own. Needs item 1 as the hand-over.

Seam: `IngestEvents` already only enqueues through the `Enqueuer` interface.
A second binary mode (`-role ingest|worker`) wires a log publisher there.

### 4. Backpressure and load shedding

Priority classes served in order (urgent first), circuit breakers per target
account, quotas per tenant.

Seam: `Message.Priority` exists and is spooled. Queues are per endpoint in
`DeliverMessages`, a priority queue replaces the channel there.

### 5. Stateful decisions: grouping, silences, escalation with acknowledgement

Group related events into one message, silence by matcher and time, flap
detection, and escalation chains: "not acknowledged in 5 minutes, then the
next channel".

Seam: fallback is the first, failure based step of escalation. Escalation by
time needs an `Acknowledge` driving port and a return channel from targets.
The Matrix target already receives and decrypts messages for chat commands
and the canary, so a reaction handler is a small step from there. State lives behind a new driven port
so it can move to the replicated store from item 2. See ADR 0003.

### 6. AI classification as an enrichment step

Let a model label events (severity, owning team, "probably noise"), and let
plain rules decide on those labels.

Seam and limits: not an expression engine (slow, costs money, not
deterministic, can be down). It runs before routing as an `Enricher` port
with a timeout and a default, adds labels such as `ai_severity`, and the
audit entry records what it said. See ADR 0002.

### 7. Audit at scale

Today: memory ring plus a rotated JSON lines file.

Next: an `AuditStore` adapter for ClickHouse or Postgres with retention,
sampling for noisy sources, and redaction of personal data before storage.

Seam: `ports.AuditStore` and `AuditStoreContract`.

### 8. Rule operations

Versioned rule sets, validation in CI with the simulator (`/api/simulate`
exists), shadow mode that routes with new rules but only records, staged
rollout, and an audit of config changes. Hot reload on SIGHUP first.

Seam: `domain.Router` is immutable and built from `domain.Spec`, so a reload
swaps one pointer.

### 9. Security

mTLS between nodes and from senders, OIDC with roles for the UI, secret
rotation without restart.

Seam: signatures and token headers per source exist. The UI already sits on
its own listener.

### 10. Observability and tests

OpenTelemetry traces from ingest to delivery, a histogram for end-to-end
latency with an SLO, load tests, chaos tests (kill nodes, cut the network).

## Connectors

Status: done, next (the coming releases), then, later, todo (wanted, not
scheduled). The phases for targets are explained in the
[README](README.md#coming-next).

| Kind | Connector | Status | Note |
| --- | --- | --- | --- |
| source | alertmanager, generic | done | |
| source | cloudevents | done | the format to use for anything new |
| source | GitHub, Gitea, Forgejo, GitLab | done | `generic` plus signature or token_header |
| source | Grafana Alerting | works | `alertmanager` kind, see docs/recipes |
| source | ntfy and Gotify publishers | done | `/ntfy`, `/gotify` |
| source | Uptime Kuma, Gatus, healthchecks | todo | `generic` works, decoders add status and labels |
| source | Kubernetes events | todo | today via kubernetes-event-exporter, later a direct watch |
| source | Sentry | todo | |
| source | AWS SNS, GCP Pub/Sub, Azure Monitor | todo | SNS needs subscription confirmation and signature checks |
| source | NATS, Kafka, MQTT | todo | needs a pull based driving adapter next to HTTP |
| source | syslog, SMTP inbound | todo | for devices that can do nothing else |
| target | matrix, webhook | done | |
| target | email (SMTP), ntfy | done | the independent second channel |
| target | Slack, Discord, Mattermost | next | incoming webhooks or bot tokens, Markdown converted per platform |
| target | Microsoft Teams | next | Workflows webhooks with Adaptive Cards |
| target | Telegram | next | Bot API, MarkdownV2 |
| target | Gotify, Pushover, UnifiedPush | then | more independent channels for fallbacks |
| target | Signal | then | through signal-cli REST API |
| target | PagerDuty | then | Events API v2, dedup key from the event |
| target | SMS and voice (Twilio or a European provider) | then | last escalation step |
| target | Jira, GitHub or Forgejo issues | later | |
| target | NATS, Kafka | later | forwarding to downstream systems |
| target | Grafana IRM, incident.io | later | |
| return | Matrix chat commands | done | mute, unmute, status, canary |
| return | acknowledge via Matrix reaction or ntfy action | todo | needs item 5 |
| return | Alertmanager silences | todo | |
| enrich | Prometheus query, Kubernetes API, CMDB lookup | todo | `Enricher` port, see item 6 |
| engine | expr-lang, jq, JavaScript | todo | `domain.Engine` plus `EngineContract` |
