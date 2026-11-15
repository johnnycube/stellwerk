# Cron jobs and scripts

## Ping URLs

A heartbeat with a `token` gets URLs like healthchecks.io: call one when
the job ran, the other when it failed. If it does not call in time, you get
an alarm.

```yaml
heartbeats:
  - name: backup
    token: "${TOKEN_BACKUP}"
    timeout: 25h
    to: critical
```

```sh
# crontab
0 3 * * *  restic backup /data && curl -fsS "https://stellwerk.example.org/ping/backup?token=$T" \
                               || curl -fsS --data-binary @/var/log/backup.log "https://stellwerk.example.org/ping/backup/fail?token=$T"
```

`/fail` alerts at once with the body as detail, the next successful ping
sends a recovery. Mute a planned outage with
`!stellwerk mute 1d heartbeat=backup`.

## stellwerk send

```sh
export STELLWERK_URL=https://stellwerk.example.org STELLWERK_TOKEN=... STELLWERK_SOURCE=scripts
stellwerk send -title "Disk check" "sda: 3 reallocated sectors"
smartctl -a /dev/sda | stellwerk send -title "SMART report" -
```

The message goes to `POST /in/<source>` as `{"title", "message"}`, a
`generic` source routes it like anything else.
