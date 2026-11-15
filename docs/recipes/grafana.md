# Grafana Alerting

Grafana's webhook contact point sends almost the same JSON as
Alertmanager, so the `alertmanager` kind reads it.

In Grafana: **Alerting, Contact points, Add contact point**, integration
**Webhook**:

- URL: `https://stellwerk.example.org/in/grafana`
- Optional settings, Authorization header: scheme `Bearer`, credentials: the token

```yaml
sources:
  grafana: { kind: alertmanager, token: "${TOKEN_GRAFANA}" }
routes:
  - name: grafana-critical
    match: { source: grafana, status: firing, labels: { severity: critical } }
    to: critical
    loud: true
  - name: grafana
    match: { source: grafana }
    to: warning
```

The built-in template shows each alert's `summary` annotation, so give your
Grafana rules one.
