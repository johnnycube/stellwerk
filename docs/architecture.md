# Architecture and testing

stellwerk is hexagonal with explicit use cases. The core knows nothing about HTTP, YAML, Matrix or clustering, and nothing reaches it except through a use case.

```
 driving adapters          driving ports = use cases              driven ports        driven adapters
                         ┌──────────────────── core ─────────────────────┐
 http /in/{source} ────► │ IngestEvents ─────┐                           │
 http UI ──────────────► │ SimulateEvents    ├─► DeliverMessages ─► Sender ──► target/matrix, email, ntfy, webhook
                    ───► │ DescribeTopology  │           │       Outbox ───► spool (SQLite)
 scheduler ────────────► │ CheckHeartbeats ──┘        Coordinator ───────────► coordination/local, peers
                         │                            AuditStore ────────────► audit/memory + file
                         │                            Recorder, StatsReader ──► stats/memory, metrics
                         │ domain: Event, Rule, Router, Renderer           Clock ─► system clock
                         │         Engine ◄──────────────────────────────────── engine/cel
                         └───────────────────────────────────────────────┘
 config (YAML) ──► domain.Spec              bootstrap wires everything
```

| Use case | Driving port | What it does |
|---|---|---|
| `IngestEvents` | `ports.IngestEvents` | heartbeat? duplicate? route, deliver, sync or async, outcome for the sender |
| `SimulateEvents` | `ports.SimulateEvents` | the same routing, without recording, deduping or sending |
| `DescribeTopology` | `ports.DescribeTopology` | rules, destinations, templates, heartbeats, cluster, stats |
| `CheckHeartbeats` | `ports.CheckHeartbeats` | missing/restored per heartbeat, every node tracks, only the leader sends |
| `DeliverMessages` | used by the above | spool, one queue per endpoint, retries with backoff, breaker, message age, fallbacks, records results |
| `DigestMessages` | used by the above | collects digest routes and sends one summary per window |
| `ManageSilences` | `ports.ManageSilences` | mute and unmute, matched by ingest, simulation and heartbeat alarms |
| `ResendAudit` | `ports.ResendAudit` | send an audit entry's messages again |
| `RunCanary` | `ports.RunCanary` | a test message through the chain, read back by a second account |
| `CheckHeartbeats` | `ports.ReceivePings` | ping URLs: ran, failed, recovered |

Driven ports: `Sender`, `Receiver`, `Outbox`, `SilenceStore`, `Coordinator`, `AuditStore`, `Recorder`, `StatsReader`, `Clock`. The domain defines `Engine` for rule expressions.

- `internal/core/domain`: events, rules, router, the `Engine` interface, templates. Pure, no I/O, no expression language.
- `internal/core/ports`: driving ports (`in.go`) and driven ports (`out.go`).
- `internal/core/ports/portstest`: fakes for every driven port and contract suites every implementation must pass.
- `internal/core/usecase`: the use cases, one file each.
- `internal/adapters/in`: HTTP (ingest, ntfy and Gotify endpoints, pings, health, UI), the source decoders, chat commands, the schedulers (heartbeats, canary, self ping).
- `internal/adapters/out`: targets, spool, audit, coordination (local, peers), stats, metrics.
- `internal/adapters/engine`: expression engines (CEL) and their contract suite.
- `internal/config`: reads YAML and maps it to `domain.Spec`.
- `internal/bootstrap`: the composition root, the only package that knows every adapter.
- `internal/archtest`: fails the build if the dependency rule breaks, if a driving adapter imports use case implementations instead of ports, or if a driving port has no use case.

## Testing

```sh
go test -tags goolm -race ./...
go test -tags "goolm noha" ./...
```

- **Domain**: every matcher, reason text, engine selection, fallback, priority, template helper and edge case (98% coverage).
- **Use cases**: scenario tests per use case against fakes on every driven port, with a manual clock, so heartbeat timing and dedupe windows are tested exactly and without sleeping. Spool recovery, shutdown in the middle of a send and fallbacks included (98% coverage).
- **Port contracts**: `portstest.CoordinatorContract`, `RecorderContract`, `SenderContract`, `ClockContract`, `AuditStoreContract`, `OutboxContract`, and `enginetest.EngineContract`. The fakes and every real adapter run the same suite: local and peers coordinators, the memory recorder and audit store, the SQLite spool (including reopening), the webhook, email and ntfy targets, the CEL engine. A new adapter is done when its contract test passes.
- **Fuzzing**: every decoder, the matcher and duration parsers (`go test -fuzz`, 30 s each in CI).
- **Process**: `test/process` builds the binary, kills it with SIGKILL during failing deliveries and checks that the restart delivers everything.
- **Docs**: every stellwerk snippet in `docs/recipes` is parsed and built, the schema in `docs/` must match the code, and the UI's script must parse.
- **Integration**: the composition root end to end over HTTP, with two accounts, sync and async, dedupe, audit log, metrics, drained shutdown, a restart that resumes from the spool, fallbacks and a two-node HA cluster.
- **End to end**: `test/e2e` runs against a real Synapse (`-tags "goolm e2e"`, see the file header). It registers users, creates encrypted rooms with its own E2EE client, and checks that two bot accounts deliver decryptable messages, that unencrypted rooms are refused, that the audit log matches, that chat commands work and only one bot answers, that the canary is decrypted by a second bot, that a room the bot is not in is reported, and that `stellwerk init` produces a working setup. CI starts Synapse in a container for it (`.github/workflows/e2e.yml`).
- **Architecture**: `internal/archtest` (see above).
