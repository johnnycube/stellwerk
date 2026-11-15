# Operations

Running stellwerk day to day: silences, chat commands, digests, ping URLs, the canary, rule tests, reloads, reliability, high availability, the audit log, metrics and the dataflow UI.

## Silences

Mute what matches for a while, from the UI (**Silences**), the API or the chat. Matchers are `source=`, `status=` or any label, regexes with `!` for negation. Silenced events show up in the audit log as `silenced`, with who muted them and why. A heartbeat alarm is muted with `heartbeat=<name>`. Silences are stored in the spool file and survive restarts. They last 30 days at most.

## Chat commands

With `commands: true` on a Matrix account, people in `command_users` (default: `allowed_inviters`) can write in its rooms:

```
!stellwerk mute 2h source=k8s severity=warning -- cluster upgrade
!stellwerk unmute s-1a2b3c4d
!stellwerk silences
!stellwerk status
!stellwerk canary
```

`!sw` works too. The bot answers encrypted in the same room. Turn commands on for one account per room, or every bot there answers. In a cluster every node applies the command, only the leader answers.

## Digests

`digest: 1h` on a route collects its messages per destination and sends one summary when the hour is over: a night of downloads becomes a few messages. The audit log shows the collected events as *waiting for the digest*. Open digests are held in memory: a clean shutdown sends them early, a crash loses them, so use digests for noise, not for alerts.

## Ping URLs for cron jobs

A heartbeat with a `token` (and no `source`, or both) gets two URLs, like healthchecks.io:

```yaml
heartbeats:
  - { name: backup, token: "${TOKEN_BACKUP}", timeout: 25h, to: critical }
```

`GET|POST /ping/backup?token=...` after each run, `/ping/backup/fail` on failure (the body becomes the detail, the alarm goes out at once, the next good ping sends a recovery). If neither arrives within the timeout, the missing alarm goes out.

## The canary

```yaml
canary:
  to: [critical]           # the destinations to prove
  at: "09:00"              # daily, local time (or every: 24h)
  verify_with: matrix/media  # optional: a second Matrix account in the same rooms must decrypt it
```

The canary goes through the real queues, retries and fallbacks. If it cannot be delivered, the destination's fallbacks get it, marked as a failure: that is the alarm. With `verify_with`, the second account must receive and decrypt it within `timeout`, which catches broken encryption, a kicked bot or a dead homeserver that still accepts messages. The result is in the UI header, `stellwerk_canary_ok` and the audit log. `!stellwerk canary` or the header button send one now.

## Watching stellwerk itself

`self_ping.url` is called every minute. When stellwerk cannot deliver (an account not ready, a breaker open, a bot not in a room) it calls `<url>/fail` with the reason instead. When stellwerk is dead, the outside service notices the silence: the one alarm stellwerk cannot raise about itself.

For Matrix accounts, stellwerk checks every minute whether the bot is in the rooms its destinations use, whether those rooms are encrypted and whether its device is cross-signed. Problems show up in the UI (destination detail), as `stellwerk_endpoint_problems` and in the self ping.

## Rule tests

```yaml
tests:
  - name: critical alerts page me
    source: alertmanager
    body: '{"version":"4","status":"firing","commonLabels":{"severity":"critical"},"alerts":[]}'
    expect: { to: [critical], loud: true, priority: urgent }
  - name: sonarr tests are dropped
    source: sonarr
    body: '{"eventType": "Test"}'
    expect: { dropped_by: arr-tests }
```

`stellwerk -test` runs them with the real decoders and rules, sends nothing and exits 1 if one fails, so a CI job can check a config change. `expect` takes `routes`, `to`, `dropped_by`, `unmatched`, `heartbeat`, `contains`, `loud` and `priority`.

## Reload, send again, send from scripts

- **Reload** without a restart: `kill -HUP`, the **Reload config** button, or `-watch 30s` (`STELLWERK_WATCH`) to reload when the files change, which is what Kubernetes does to mounted ConfigMaps and Secrets. Sources, rules, templates, destinations and heartbeats reload. Changes to targets, listeners, delivery, HA, audit or logging need a restart, and the reload is refused whole, so you never run half a config.
- **Send again** from an audit entry, all of it or only what failed, after a target was fixed. It goes to the destinations as configured now.
- **`stellwerk send`** posts from scripts: `stellwerk send -source backup -title "Backup done" "42 GB"`, or `- ` to read stdin. URL, source and token come from `STELLWERK_URL`, `STELLWERK_SOURCE` and `STELLWERK_TOKEN`.

## ntfy and Gotify compatible endpoints

| Point the app at | stellwerk source |
|---|---|
| ntfy server `https://stellwerk.example.org/ntfy`, topic = source name | `kind: ntfy`, token = access token or Basic password |
| Gotify server `https://stellwerk.example.org/gotify`, app token = source token | `kind: gotify` |

ntfy publishing by `PUT`/`POST /ntfy/<topic>`, the JSON publish to `/ntfy`, `GET .../publish?message=` and `?auth=` are understood, as are Gotify's `X-Gotify-Key`, `?token=` and JSON or form bodies. Title, message, priority and tags become data and labels the rules can match. See the [recipes](recipes) for app by app setups.

## Reliability

- **Retries that listen.** Senders classify errors: permanent ones (404, 403, a 550 from the mail server, a Matrix room the bot is not in) are not retried, `429` and Matrix rate limits are retried after the time the server asks for, the rest backs off exponentially with ±20 % jitter.
- **Circuit breaker.** After `breaker_failures` (3) deliveries to an account failed in a row, it is skipped for `breaker_cooldown` (1m): messages go to fallbacks at once instead of waiting through every retry. `stellwerk_endpoint_breaker_open` shows it.
- **Late and stale messages.** A message sent later than `late_after` (10m) after it came in gets a note with its age. One older than `max_age` (off by default) is not sent at all, so a long outage does not end in a flood of stale "firing" alerts.
- **Tested the hard way.** CI kills the binary with SIGKILL in the middle of failing deliveries and checks that the restarted one delivers everything (`test/process`), fuzzes every decoder, runs `govulncheck`, and runs the Matrix parts against a real Synapse.

Matrix keys live in the database files under `/data` (one per account and node). Back them up together with the `pickle_key`: without both, the bot comes back as a new device that cannot read old messages, and every room needs to trust it again.

## High availability (optional)

A single node is the default and needs nothing extra. For HA, set `ha.enabled: true` and run the same config on two or three nodes behind a load balancer, only `node_id` differs. See [`deploy/ha`](../deploy/ha/README.md).

- **Delivery**: with `delivery.mode: sync` a node answers only after delivery. If it dies or a target fails, the sender gets an error and retries, possibly on another node.
- **Heartbeats**: every node knows the last heartbeat, however it arrived. Only the leader (lowest live `node_id`) raises alerts.
- **Dedupe**: a payload delivered by one node is remembered on all nodes for `dedupe_window`.
- **Matrix**: each node is its own device per account, cross-signed with that account's recovery key.
- **Semantics**: at-least-once. During a network split two nodes may both lead and both alert. A duplicate is better than silence.
- **Per node**: spool, audit log, open digests and silences. A chat command reaches every node and they all apply it with the same ID. A silence from the UI or API applies to the node that got the request, so in a cluster mute from the chat. Shared silences are on the [roadmap](../ROADMAP.md).

With HA off, the `/cluster/` API does not exist. Built with `-tags noha`, the binary has no cluster code and refuses `ha.enabled: true`.

## Audit log

Every received message gets an entry: when it came in, from which source, its labels and payload, what each rule decided and why, and for every destination each send attempt with its error, until it is sent or given up. Requests turned away before routing (bad payload, rate limit, full queue) are in it too, with the reason. Requests with a wrong token are not, since anyone could flood the log with them. Heartbeat alerts that stellwerk sends itself have entries as well.

The **Audit log** section of the UI lists the entries with filters for source, verdict and text (payload, labels, reasons, errors). Clicking one shows the whole story and lights up its path in the yard. `GET /api/audit` and `GET /api/audit/{id}` on the UI listener return the same as JSON.

```yaml
audit:
  enabled: true        # default
  size: 1000           # entries kept in memory for the UI
  payload: true        # keep request bodies, set false if they hold secrets
  payload_max: 16384   # bytes per body
  file: /data/audit.jsonl   # optional: one JSON line per finished entry, rotated at file_max_mb
  log: false           # optional: one log line per finished entry, for Loki & co.
```

The file and log get an entry once it is finished (all deliveries sent or given up). Each node keeps its own audit log. With HA, look at the node that received the message, or collect the files or logs centrally.

## Metrics and limits

`metrics.listen: ":9090"` serves Prometheus metrics at `/metrics`: events by source and verdict, rule matches, deliveries by destination and result, pending deliveries, heartbeat state and last seen time, endpoint readiness, leader and peer state, audit log size, deliveries waiting in the spool, and requests refused by rate limits.

A source can be limited per name: `sources.sonarr.rate_limit: 60/m` (also `/s`, `/h`), with an optional `burst` (default: the count). Over the limit the sender gets `429` with `Retry-After`. The limit is checked after the token, so nobody without a token can use up a sender's budget.

## Dataflow UI

Open `http://127.0.0.1:8081` (`ssh -L 8081:127.0.0.1:8081 host`). Sources run into one main line, each rule is a switch branching off to its destinations, drop rules end in a red buffer stop, heartbeats sit on top. Destinations show which account sends. With HA on, the header shows the nodes and the leader. **Try a payload** shows the path a body takes, why each rule did or did not match, and the message each destination would get. Nothing is sent.
