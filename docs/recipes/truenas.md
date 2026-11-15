# TrueNAS

TrueNAS has no generic webhook alert service, but its **Slack** service
posts `{"text": ...}` to any URL, which the `generic` kind reads.

In TrueNAS: **Alerts (bell), Alert Settings, Add**, type **Slack**:

- Webhook URL: `https://stellwerk.example.org/in/truenas?token=<token>`
- Level: Warning or higher
- **Send Test Alert**

```yaml
sources:
  truenas: { kind: generic, token: "${TOKEN_TRUENAS}" }
routes:
  - name: truenas
    match: { source: truenas }
    expr: 'data.text.matches("(?i)critical|degraded|failed|faulted")'
    to: critical
    loud: true
  - { name: truenas-rest, match: { source: truenas }, to: misc }
```

The token sits in the URL here, so use a token only for this source.
