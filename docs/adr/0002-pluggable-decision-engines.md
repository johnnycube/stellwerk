# 0002: Rule expressions are engines behind a port

Status: accepted, November 2026

## Context

Routes have an optional expression next to the declarative match. It was CEL,
wired into the domain. Others may prefer expr-lang, jq or JavaScript, and AI
classification comes up regularly.

## Decision

`domain.Engine` compiles an expression into a `domain.Predicate`. The router
only knows these two interfaces. Engines live in `internal/adapters/engine`,
register themselves, and every engine passes `enginetest.EngineContract`.
CEL is the built-in default, `engine:` at the top level changes the default,
`engine:` on a route picks one per rule.

A predicate must be pure: no I/O, the same event gives the same answer, the
cost is bounded. The architecture test forbids expression libraries in the
domain and I/O in engines.

## Why AI is not an engine

A model call is slow, costs money, can be down and does not answer the same
way twice. Inside the router that would make routing unexplainable, the
simulator would have side effects and an outage of the model would stop all
alerts.

AI belongs in an enrichment step before routing: an `Enricher` port with a
timeout and a default, which adds labels (`ai_severity`, `ai_team`). Rules
then match those labels like any other, the audit entry shows what the model
said, and when the model is unreachable the event is routed without its
labels.

## Consequences

- A new language is one package plus a contract test, no core change.
- Rules stay explainable: the trace says which condition failed.
- Expressions in one config can mix engines. The UI shows the engine per rule.
