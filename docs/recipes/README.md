# Recipes

Copy and paste setups for apps that should send to stellwerk. Each recipe
has what to set in the app and the stellwerk part: a source, a route and
sometimes a template. Destinations (`critical`, `misc`, ...) are the ones
from your own config.

Many apps can already notify ntfy or Gotify. stellwerk speaks both, so for
those apps you only change the server URL:

| The app supports | Server URL in the app | stellwerk source |
| --- | --- | --- |
| ntfy | `https://stellwerk.example.org/ntfy`, topic = source name | `kind: ntfy` |
| Gotify | `https://stellwerk.example.org/gotify`, app token = source token | `kind: gotify` |
| a webhook with JSON | `https://stellwerk.example.org/in/<source>` | `kind: generic` |

| App | How |
| --- | --- |
| [Alertmanager](alertmanager.md) | webhook receiver |
| [Grafana Alerting](grafana.md) | webhook contact point |
| [Uptime Kuma](uptime-kuma.md) | ntfy notification |
| [Sonarr, Radarr and the other *arr apps](arr.md) | webhook connection (or ntfy, Gotify) |
| [Proxmox VE](proxmox.md) | Gotify notification target |
| [TrueNAS](truenas.md) | Slack alert service |
| [Home Assistant](home-assistant.md) | `rest_command` |
| [Watchtower](watchtower.md) | shoutrrr generic webhook |
| [Diun](diun.md) | webhook notifier |
| [GitHub, Gitea, Forgejo](git.md) | signed repository webhooks, and Actions steps |
| [Cron jobs and scripts](cron.md) | ping URLs and `stellwerk send` |

Check a recipe without sending anything: open the UI, **Try a payload**,
pick the source and paste a sample body. Or add it to the `tests:` section
of your config and run `stellwerk -test`.
