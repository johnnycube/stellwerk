# Architecture decision records

Short notes on decisions that shape later work. Each one says what was
decided, why, and what it allows or rules out.

| No. | Decision |
| --- | --- |
| [0001](0001-durable-outbox.md) | Deliveries go through an outbox port, SQLite now, a replicated log later |
| [0002](0002-pluggable-decision-engines.md) | Rule expressions are engines behind a port, AI is enrichment, not an engine |
| [0003](0003-fallback-and-priority.md) | Fallback belongs to the destination, priority to the message |
| [0004](0004-source-authentication.md) | Sources authenticate with a token, an HMAC signature, or both |
| [0005](0005-silences-and-chatops.md) | Silences are a use case, chat is just another driving adapter |
| [0006](0006-delivery-reliability.md) | Senders classify errors, the core decides retries, breaker and message age |
