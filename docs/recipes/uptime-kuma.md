# Uptime Kuma

Uptime Kuma's ntfy notification works unchanged.

In Uptime Kuma: **Settings, Notifications, Setup Notification**, type
**ntfy**:

- Server URL: `https://stellwerk.example.org/ntfy`
- Topic: `uptime` (the source name in stellwerk)
- Priority: as you like, it arrives as the label `priority` (1 to 5)
- Authentication: access token, the source's token

```yaml
sources:
  uptime: { kind: ntfy, token: "${TOKEN_UPTIME}" }
routes:
  - name: uptime-down
    match: { source: uptime }
    expr: 'int(labels.priority) >= 4 || data.message.contains("Down")'
    to: critical
    loud: true
  - name: uptime
    match: { source: uptime }
    to: misc
```

Uptime Kuma's **push monitor** works the other way round and is a good
place for stellwerk's own `self_ping`: create a push monitor and set its
URL as `self_ping.url`.
