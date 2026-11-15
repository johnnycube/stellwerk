# Sonarr, Radarr and the other *arr apps

The **Webhook** connection sends the richest data (series, episodes, movie,
quality), so the templates can say exactly what happened.

In Sonarr or Radarr: **Settings, Connect, +, Webhook**:

- URL: `https://stellwerk.example.org/in/sonarr`
- Method: POST
- Username: anything, Password: the source's token (it is checked as Basic
  auth password)
- Pick the events you want, **Test**, save.

```yaml
sources:
  sonarr: { kind: generic, token: "${TOKEN_SONARR}", options: { status_field: eventType } }
  radarr: { kind: generic, token: "${TOKEN_RADARR}", options: { status_field: eventType } }
templates:
  sonarr: |-
    📺 **{{ get .Data "eventType" }}**: {{ get .Data "series.title" }}{{ range get .Data "episodes" }}
    - S{{ get . "seasonNumber" }}E{{ get . "episodeNumber" }} {{ get . "title" }}{{ end }}
  radarr: |-
    🎬 **{{ get .Data "eventType" }}**: {{ get .Data "movie.title" }}{{ with get .Data "movie.year" }} ({{ . }}){{ end }}
routes:
  - name: arr-tests
    match: { source: "*arr", status: Test }
    drop: true
  - name: arr-health
    match: { source: "*arr", status: "Health|HealthRestored|ApplicationUpdate" }
    to: warning
  - { name: sonarr, match: { source: sonarr }, to: media, template: sonarr, digest: 1h }
  - { name: radarr, match: { source: radarr }, to: media, template: radarr, digest: 1h }
```

`digest: 1h` turns a night of downloads into one message per hour.

The apps also have **ntfy** and **Gotify** connections. They work with the
compatible endpoints (`/ntfy`, `/gotify`), but carry only title and text.
