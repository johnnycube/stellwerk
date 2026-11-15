# 0006: Senders classify errors, the core decides what to do

Status: accepted, November 2026

## Decision

A sender wraps its errors with `ports.Permanent(err)` or
`ports.RetryAfter(err, d)`. Anything else counts as transient. The core
does not retry permanent errors, waits as long as a backend asks (at most
10 minutes), and otherwise backs off exponentially with ±20 % jitter.

A circuit breaker per account opens after `breaker_failures` deliveries in
a row finally failed and skips the account for `breaker_cooldown`. While
open, messages fail at once and go to fallbacks. Permanent errors do not
count: one bad address must not take the whole account offline.

Every delivery carries the time it was accepted. A fallback keeps the time
of the delivery it stands in for. A late message gets a note with its age,
one older than `max_age` is not sent and gets no fallback.

## Why in the core

The rules are the same for every target, and only the target knows what
its errors mean (an SMTP 550, a Matrix `M_FORBIDDEN`). Splitting it this way
keeps targets small and the policy testable with a manual clock.

## Consequences

- A new target should classify its errors. If it does not, it still works,
  just with more retries than needed.
- The breaker state is per node.
