# Watchtower

Watchtower sends through shoutrrr. shoutrrr's ntfy service cannot use a
path like `/ntfy`, so use its generic webhook with the `generic` kind. The
`@authorization` parameter becomes a header.

```yaml
# docker compose
services:
  watchtower:
    image: containrrr/watchtower
    environment:
      WATCHTOWER_NOTIFICATIONS: shoutrrr
      WATCHTOWER_NOTIFICATION_URL: "generic+https://stellwerk.example.org/in/watchtower?@authorization=Bearer+<token>"
```

```yaml
# stellwerk.yaml
sources:
  watchtower: { kind: generic, token: "${TOKEN_WATCHTOWER}" }
routes:
  - { name: updates, match: { source: watchtower }, to: misc, digest: 6h }
```
