# 0003: Fallback belongs to the destination, priority to the message

Status: accepted, November 2026

## Context

With Matrix as the only channel, a homeserver outage silences every alert.
The first step towards escalation is "if it cannot be delivered, try
another way".

## Decision

`fallback` is set on a destination, not on a route. It means "when this place
is unreachable, use that one", which is a property of the place: every route
and every heartbeat alarm that sends there gets the same protection without
repeating it.

A fallback starts when the last retry failed, not on the first error. It is
one level deep: a fallback's own fallback is not used, so chains cannot loop.
The fallback message is marked with the destination it stands in for. In
sync mode the sender gets success if the destination or one of its
fallbacks delivered. The audit entry lists the fallback deliveries and gets
the verdict `fallback`.

Priority (`low`, `normal`, `high`, `urgent`) is part of the message. Routes
set it, the default follows `loud`. A missing heartbeat is `urgent`. Targets
map it to what they have, and it is stored in the spool so later queues can
serve urgent messages first.

## Consequences

- Escalation by time (unacknowledged after five minutes) is a separate,
  later feature. It needs acknowledgement from targets and state, see the
  roadmap. Fallback by failure needs neither.
- If two destinations of one event fail and share a fallback, the fallback
  gets two messages. That is visible and rare, deduplicating it would hide
  which destination failed.
