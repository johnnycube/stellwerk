# Two or three nodes

High availability is optional and off by default. A single node needs none of this.

Run the same `stellwerk.yaml` on every node. Only the node ID differs: set `STELLWERK_NODE_ID` per node (on Kubernetes the StatefulSet in `deploy/kubernetes` does it from the pod name), and the shared secret via `STELLWERK_HA__SECRET` or a Secret key `ha.secret`.

```yaml
ha:
  enabled: true
  peers:
    - { id: a, url: "https://a.internal:8080" }
    - { id: b, url: "https://b.internal:8080" }
    - { id: c, url: "https://c.internal:8080" }
```

- Nodes reach each other on `/cluster/v1/*` with the shared secret. Use a private network, WireGuard or TLS between them.
- Put a load balancer (Caddy, HAProxy, DNS round robin with health checks on `/readyz`) in front of all nodes. Senders use the load balancer URL.
- Each node is its own Matrix device per account: keep `{node}` in `database` and set `recovery_key` per account, so new devices cross-sign themselves.
- Binaries built with `-tags noha` contain no cluster code and refuse `ha.enabled: true`.
- Keep clocks in sync (NTP). Heartbeat times are exchanged as timestamps.
- Use `delivery.mode: sync` (default). If a node dies mid-request, the sender gets an error and retries on another node.

Example Caddyfile for the load balancer:

```
alerts.example.org {
  reverse_proxy a.internal:8080 b.internal:8080 c.internal:8080 {
    lb_policy first
    health_uri /readyz
    health_interval 5s
  }
}
```
