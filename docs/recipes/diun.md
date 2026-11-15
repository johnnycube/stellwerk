# Diun

Diun's webhook notifier posts one JSON document per image.

```yaml
# diun.yml
notif:
  webhook:
    endpoint: https://stellwerk.example.org/in/diun
    method: POST
    headers:
      content-type: application/json
      authorization: Bearer <token>
```

```yaml
# stellwerk.yaml
sources:
  diun: { kind: generic, token: "${TOKEN_DIUN}", options: { status_field: status } }
templates:
  diun: |-
    🐳 **{{ get .Data "image" }}** {{ if eq .Status "new" }}is new{{ else }}was updated{{ end }} on {{ get .Data "hostname" }}{{ with get .Data "hub_link" }}
    {{ . }}{{ end }}
routes:
  - { name: images, match: { source: diun }, to: misc, template: diun, digest: 24h }
```
