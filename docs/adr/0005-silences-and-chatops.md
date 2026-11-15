# 0005: Silences are a use case, chat is just another driving adapter

Status: accepted, November 2026

## Decision

Silences are a use case (`ManageSilences`) behind a driving port, with a
driven `SilenceStore`. The UI, the HTTP API and chat commands are three
adapters calling the same port. Ingest, the simulator and the heartbeat
alarm ask the use case whether an event is muted, after routing, so the
audit entry still shows which rule would have sent it and why it did not.

Chat commands live in `adapters/in/chatops`, which knows nothing about
Matrix. The Matrix target hands it the text of messages from allowed users
and sends back the reply. Another chat target can do the same.

A silence needs at least one matcher, lasts 30 days at most, and is stored
in the spool file when there is one.

## Cluster behaviour

Every node of a cluster receives a chat message. All of them apply the
command: the silence ID is derived from the chat message's ID, so every
node creates the same silence and a repeat changes nothing. Only the leader
answers and only the leader runs the canary. A UI or API request reaches
one node only, which is why shared silences are on the roadmap.

## Consequences

- Muting is visible: silenced events are in the audit log with the silence,
  its author and comment.
- Acknowledgement and escalation can follow the same pattern: a port, the
  chat adapter calls it.
