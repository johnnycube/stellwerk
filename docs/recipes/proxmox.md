# Proxmox VE

Proxmox VE 8.1 and later have a Gotify notification target, which stellwerk
accepts as is.

In Proxmox: **Datacenter, Notifications, Add, Gotify**:

- Server: `https://stellwerk.example.org/gotify`
- Token: the token of a `gotify` source in stellwerk

Then point a **matcher** at it (all events, or `severity: warning, error`).

```yaml
sources:
  proxmox: { kind: gotify, token: "${TOKEN_PROXMOX}" }
routes:
  - name: proxmox-errors
    match: { source: proxmox }
    expr: 'int(labels.priority) >= 8'
    to: critical
    loud: true
  - name: proxmox
    match: { source: proxmox }
    to: misc
```

Gotify priorities are numbers. Check what your Proxmox sends for errors and
backups with **Try a payload** or the audit log, then adjust the expression.
Proxmox VE 8.3 also has a generic webhook target, `kind: generic` reads that.
