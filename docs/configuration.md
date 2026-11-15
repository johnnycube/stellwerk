# Configuration

Everything about `stellwerk.yaml`: where values come from, targets and accounts, fallbacks, the spool, source kinds, expressions and templates. Start with the [onboarding guide](../README.md#onboarding) if you have no config yet.

See [`stellwerk.example.yaml`](../stellwerk.example.yaml). Editors with the YAML language server complete and check it against [`docs/stellwerk.schema.json`](stellwerk.schema.json) (the example's first line points there). `stellwerk -schema` prints the schema.

| Section | What it is |
|---|---|
| `sources` | Who may send: `kind`, `token` (optional `token_header`) and/or `signature`, optional `rate_limit` and `burst`, decoder `options`. `"*"` catches unlisted names. |
| `targets` | Output systems: `type`, shared `settings`, and optional `accounts` whose settings are merged over the shared ones. Without `accounts` there is one account, `default`. |
| `destinations` | Named places: `target`, `account` (optional if the target has one), `address`, `fallback`. |
| `routes` | Ordered rules: `match`, `expr`, `engine`, `to`, `template`, `loud`, `priority`, `mention`, `drop`, `continue`. |
| `engine` | Default expression engine for `expr`, `cel`. |
| `heartbeats` | Events that must keep arriving, with a timeout and where to alert. |
| `delivery` | `sync` or `async`, timeout, retries, `dedupe_window`, `drain_timeout` for shutdown, `spool` file. |
| `ha` | `enabled` (default false), `secret`, `peers`. |
| `ui` | Listen address and optional password for the dataflow UI. |
| `log` | `level` (debug, info, warn, error) and `format` (text, json). |
| `audit` | The audit log, see below. |
| `metrics` | `listen` address for Prometheus metrics, empty = off. |
| `self_ping` | `url` of an outside check (healthchecks.io, Uptime Kuma push) that stellwerk calls every minute, `/fail` with the reason when it cannot deliver. |
| `canary` | `to`, `every` or `at`, `verify_with`, `alert`: a test message through the whole chain, see below. |
| `tests` | Rule tests run by `stellwerk -test`, see below. |

Senders `POST /in/<source>` with the token as `Authorization: Bearer`, Basic auth password (Sonarr/Radarr) or `?token=`. Query parameters become labels for `generic` sources.

Senders that sign their webhooks need no token in the URL:

```yaml
sources:
  forgejo: { kind: generic, signature: { preset: forgejo, secret: ... } }    # also github, gitea
  custom:  { kind: generic, signature: { header: X-Signature, prefix: "sha256=", secret: ... } }
  gitlab:  { kind: generic, token: ..., token_header: X-Gitlab-Token }
```

The signature is an HMAC-SHA256 of the body, hex encoded. With `token` and `signature` both set, both must hold.

## Where config comes from

Every key can come from three layers. Each layer overrides the one before, so a file holds the rules, env tweaks them, and mounted secrets always win.

| Layer | How | Example |
|---|---|---|
| 1. Config files | The YAML file (`-config` or `STELLWERK_CONFIG`, optional, `${VAR}` is expanded), then `*.yaml` fragments in the config directories. | `stellwerk.yaml`, a ConfigMap with `rules.yaml` |
| 2. Environment | `STELLWERK_` + key path with `__` between levels. `STELLWERK_<PATH>_FILE` reads the value from a file and beats the plain variable. | `STELLWERK_DELIVERY__MODE=async`, `STELLWERK_TARGETS__MATRIX__ACCOUNTS__ALERTS__PASSWORD_FILE=/run/secrets/pw` |
| 3. Secret files | Every other file in the config directories (`-config-dir` or `STELLWERK_CONFIG_DIR`, comma-separated) is one value, named by its key path. | a Secret mounted at `/etc/stellwerk/secrets` with the key `targets.matrix.accounts.alerts.password` |

- Names are case-insensitive and `_` matches `-`, so `STELLWERK_SOURCES__K8S_EVENTS__TOKEN` sets `sources.k8s-events.token`.
- `*` cannot appear in env names or Secret keys, write `_any`: `STELLWERK_SOURCES___ANY__TOKEN`, `sources._any.token`.
- Lists and maps: a value starting with `[` or `{` is read as YAML, e.g. `STELLWERK_HA__PEERS=[{id: a, url: "http://a:8080"}]`. Single entries: `STELLWERK_HA__PEERS__1__URL=...`, `STELLWERK_ROUTES__0__TO=pager`.
- Unknown keys inside a known section are errors. Variables whose first part is no config key are ignored with a warning that suggests the right spelling. Kubernetes service links (`STELLWERK_SERVICE_HOST`, `STELLWERK_PORT...`) are ignored silently.
- `stellwerk -print-config` shows the merged result with passwords, tokens and keys redacted.

On Kubernetes, see [`deploy/kubernetes/stellwerk.yaml`](../deploy/kubernetes/stellwerk.yaml): a ConfigMap for rules, a Secret for values, a StatefulSet whose pod names become node IDs, a volume per pod for the Matrix keys, and a drained shutdown within `terminationGracePeriodSeconds`.

## Targets and accounts

```yaml
targets:
  matrix:
    type: matrix
    settings:                       # shared by all accounts
      homeserver: https://matrix.example.org
      database: /data/{target}-{account}-{node}.db
    accounts:
      alerts: { user_id: "@stellwerk:example.org", password: ..., pickle_key: ..., recovery_key: ... }
      media:  { user_id: "@mediabot:example.org",  password: ..., pickle_key: ..., recovery_key: ... }
destinations:
  critical: { target: matrix, account: alerts, address: "!aaaa:example.org" }
  media:    { target: matrix, account: media,  address: "!dddd:example.org" }
```

| Type | Address | Settings |
|---|---|---|
| `matrix` | room ID | `homeserver`, `user_id`, `password`, `database` (`{target}`, `{account}`, `{node}` are replaced), `pickle_key`, `recovery_key`, `allowed_inviters`, `device_name` |
| `webhook` | path appended to `url` | `url`, `headers`, `timeout`. Body: `source`, `route`, `markdown`, `loud`, `mentions`, `account`, `node`, `time`. |
| `email` | recipients, comma separated | `host`, `port`, `tls` (`starttls` default, `implicit`, `none`), `username`, `password`, `from`, `subject_prefix`, `timeout`. Plain text, subject from the first line. |
| `ntfy` | topic | `url` (default `https://ntfy.sh`), `token` or `username` and `password`, `tags`, `timeout`. First line becomes the title. |

`loud: true` (or any `mention`) makes the target notify. In Matrix that is `m.text` with intentional mentions, quiet messages are `m.notice`, which default push rules ignore.

`priority: low | normal | high | urgent` sets how important a message is. Default: `high` if loud, else `normal`, a missing heartbeat is `urgent`. ntfy maps it to its priorities 2 to 5, mail marks `high` and `urgent` as important.

## Fallbacks

```yaml
destinations:
  critical: { target: matrix, account: alerts, address: "!aaaa:example.org", fallback: [phone, inbox] }
  phone:    { target: push, address: my-alerts }
  inbox:    { target: mail, address: me@example.org }
```

When delivery to `critical` finally fails (all retries used), `phone` and `inbox` get the message with a note at the end that it is a fallback. Every route and heartbeat that sends to `critical` is covered. A fallback's own fallback is not used. In sync mode the sender gets success if a fallback delivered, the audit log shows the verdict `delivered via fallback`.

## Spool

```yaml
delivery:
  spool: /data/spool-{node}.db
```

A SQLite file that holds every accepted delivery until it is sent or given up. In async mode the `202` means "on disk". After a restart or crash, what is left is sent first, with a new audit entry. If the disk is full, stellwerk answers `503` instead of accepting and losing. Delivery is at-least-once: a crash right after a send can send that message again.

## Source kinds

| Kind | Payload | Options |
|---|---|---|
| `alertmanager` | Alertmanager webhook v4. Labels are common + group labels, status is firing/resolved. | `split_alerts: true` makes one event per alert with its own labels. |
| `generic` | Any JSON, or plain text as `{"text": ...}`. | `status_field: eventType`, `labels: { host: server.name }` |
| `cloudevents` | CloudEvents 1.0: structured, batch or binary (`ce-*` headers). `type`, `source`, `subject`, `time` and extensions become labels, `source#id` is the dedupe key. | `status_field: data.state`, otherwise an extension `status` |

## Expressions

`expr:` is ANDed with `match:`. It is compiled by an expression engine: `cel` is built in and the default. `engine:` at the top level changes the default, `engine:` on a route picks one per rule. Other engines plug in, see Extending.

CEL variables: `source`, `status`, `labels` (map), `data` (the payload). Integral numbers are ints, string extensions are on.

```yaml
expr: 'labels.severity in ["critical", "page"] && size(data.alerts) >= 3'
expr: 'data.?reason.orValue("") in ["OOMKilling", "OOMKilled"] || data.?count.orValue(0) >= 10'
```

A missing key makes the expression fail, which counts as no match. Use `has(data.x)` or `data.?x.orValue(default)`. Expressions are type-checked at startup.

Engines must be pure: no I/O and the same answer for the same event. That is why AI classification is planned as an enrichment step before routing that adds labels, not as an engine, see [ADR 0002](adr/0002-pluggable-decision-engines.md).

## Templates

Event fields: `.Source`, `.Kind`, `.Status`, `.Labels`, `.Data`. Helpers: `get .Data "a.b.0.c"`, `json`, `upper`, `lower`, `join`, `default`, `since`. Without `template:` the built-in template named like the source kind is used, otherwise `generic`.
