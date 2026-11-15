# 0001: Deliveries go through an outbox port

Status: accepted, November 2026

## Context

Before this, queued deliveries lived in memory only. In async mode stellwerk
answered `202 Accepted` and then could lose the message on a crash or a
restart that ran into the drain timeout. A sender that got a 2xx does not
send again.

## Decision

`ports.Outbox` with three calls: `Put` (returns once durable), `Done` and
`Pending`. `DeliverMessages` puts every delivery before `Enqueue` returns,
removes it when it is finished (sent or finally failed), and reads what is
left when it is wired, before any new work is accepted. A delivery stopped by
shutdown stays in the outbox and is sent after the restart.

The first adapter is a SQLite file per node (`delivery.spool`), WAL with
`synchronous=FULL`, so an accepted delivery survives power loss. The stored
format is a versioned record type in the adapter, not the domain struct.

Each delivery gets an ID when it is enqueued. It is the key in the outbox and
the idempotency key for anything that replays.

## Consequences

- At-least-once: a crash between send and `Done` sends the message again.
  Duplicates are preferred over losses for alerts.
- A full spool disk refuses work (`503`), it does not accept and lose.
- Deliveries for an endpoint that no longer exists after a config change stay
  in the spool and are logged, nothing is dropped silently.
- A replicated log (NATS JetStream) implements the same port: publish,
  ack and the unacked backlog. Nothing in the core changes for that.
