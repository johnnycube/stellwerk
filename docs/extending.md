# Extending stellwerk

New source kinds, target types, expression engines, spools, coordinators and use cases. Every extension point is a port with a contract suite: an adapter is done when its contract test passes.

## A new source kind

```go
// internal/adapters/in/source/grafana.go
package source

func init() {
	Register(Info{Kind: "grafana", Description: "Grafana alerting webhook", Sample: `{...}`},
		func(opts *yaml.Node) (Decoder, error) { return &Grafana{}, nil })
}

func (g *Grafana) Decode(in Input) ([]*domain.Event, error) {
	data, err := DecodeJSON(in.Body)
	return []*domain.Event{{Source: in.Source, Kind: "grafana", Status: "...", Labels: map[string]string{}, Data: data}}, err
}
```

## A new target type

```go
// internal/adapters/out/target/gotify/gotify.go
package gotify

func init() {
	target.Register(target.Info{Type: "gotify", Description: "Gotify app", Address: "unused"},
		func(ctx context.Context, env target.Env, n *yaml.Node) (target.Target, error) {
			var s Settings // already merged: shared settings + this account's
			return &Gotify{s: s}, target.DecodeSettings(n, &s)
		})
}

func (t *Gotify) Send(ctx context.Context, address string, msg domain.Message) error { ... }
```

Add the import to `internal/adapters/out/target/all/all.go`. Accounts come for free. Optional interfaces: `Run(ctx)`, `Ready() bool`, `Close() error`. `ntfy` is a complete small example.

Then add a contract test next to it:

```go
func TestGotifySenderContract(t *testing.T) {
	portstest.SenderContract(t, func(t *testing.T) portstest.SenderUnderTest { ... })
}
```

## A new expression engine

```go
// internal/adapters/engine/expr/expr.go
package expr

func init() { engine.Register(Engine{}) }

func (Engine) Name() string { return "expr" }

func (Engine) Compile(src string) (domain.Predicate, error) { ... } // must be pure, see ADR 0002
```

Import it in `internal/adapters/engine/all`, run `enginetest.EngineContract` against it, and use it with `engine: expr` on a route or at the top level.

## Another spool

Implement `ports.Outbox` (e.g. on NATS JetStream), pass `portstest.OutboxContract`, and open it in `internal/bootstrap`.

## Another coordinator

Implement `ports.Coordinator` (e.g. backed by Redis or etcd) under `internal/adapters/out/coordination/`, make it pass `portstest.CoordinatorContract`, and select it in `internal/bootstrap`.

## A new use case

Add a driving port interface to `internal/core/ports/in.go`, implement it in `internal/core/usecase` with a `var _ ports.X = (*X)(nil)` assertion and scenario tests, then call it from an adapter. The architecture test fails until all three exist.
