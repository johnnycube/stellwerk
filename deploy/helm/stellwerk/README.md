# Helm chart

```sh
helm install stellwerk oci://ghcr.io/johnnycube/charts/stellwerk -f my-values.yaml
```

`config` is the stellwerk configuration without secrets. `secrets` holds one
key per secret value, named by its config path, or point `existingSecret` at
a Secret you manage. The chart adds what belongs to Kubernetes: listen
addresses, the data volume, the spool and audit file, JSON logs, node IDs
from the pod names, the HA peer list when `ha.replicas` is above 1, and a
reload when the ConfigMap or Secret changes.

```yaml
# my-values.yaml
config:
  sources:
    alertmanager: { kind: alertmanager }
  targets:
    matrix:
      type: matrix
      settings: { homeserver: https://matrix.example.org, allowed_inviters: ["@me:example.org"] }
      accounts:
        alerts: { user_id: "@stellwerk:example.org", commands: true }
  destinations:
    alerts: { target: matrix, account: alerts, address: "!aaaa:example.org" }
  routes:
    - { name: everything, to: alerts }
secrets:
  sources.alertmanager.token: change-me
  targets.matrix.accounts.alerts.password: change-me
  targets.matrix.accounts.alerts.pickle_key: a-long-random-string-never-change-it
```
