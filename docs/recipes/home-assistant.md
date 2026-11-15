# Home Assistant

A `rest_command` is the most direct way and works in every version.

```yaml
# configuration.yaml
rest_command:
  stellwerk:
    url: https://stellwerk.example.org/in/homeassistant
    method: POST
    headers:
      authorization: !secret stellwerk_bearer   # "Bearer <token>" in secrets.yaml
    content_type: application/json
    payload: '{"title": {{ title | tojson }}, "message": {{ message | tojson }}, "priority": {{ priority | default("normal") | tojson }}}'
```

Use it in automations:

```yaml
actions:
  - action: rest_command.stellwerk
    data:
      title: Water leak
      message: The sensor under the sink is wet.
      priority: urgent
```

```yaml
# stellwerk.yaml
sources:
  homeassistant: { kind: generic, token: "${TOKEN_HA}" }
routes:
  - name: home-urgent
    match: { source: homeassistant, fields: { priority: urgent } }
    to: critical
    loud: true
    priority: urgent
  - { name: home, match: { source: homeassistant }, to: misc }
```

Home Assistant's ntfy integration should also work against
`https://stellwerk.example.org/ntfy` with a source of `kind: ntfy`.
