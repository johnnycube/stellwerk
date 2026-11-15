# Alertmanager

```yaml
# alertmanager.yml
receivers:
  - name: stellwerk
    webhook_configs:
      - url: https://stellwerk.example.org/in/alertmanager
        send_resolved: true
        http_config:
          authorization: { credentials: <token> }
route:
  receiver: stellwerk
  routes:
    - matchers: [alertname = Watchdog]   # the heartbeat, every minute
      receiver: stellwerk
      repeat_interval: 1m
```

```yaml
# stellwerk.yaml
sources:
  alertmanager: { kind: alertmanager, token: "${TOKEN_ALERTMANAGER}" }
routes:
  - name: critical
    match: { source: alertmanager, status: firing, labels: { severity: "critical|page" } }
    to: critical
    loud: true
  - name: alerts
    match: { source: alertmanager }
    to: warning
heartbeats:
  - { name: watchdog, source: alertmanager, labels: { alertname: Watchdog }, timeout: 10m, to: critical }
```

`options: { split_alerts: true }` on the source makes one message per alert
instead of one per group.
