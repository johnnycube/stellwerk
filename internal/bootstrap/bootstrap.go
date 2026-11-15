// Package bootstrap is the composition root: the only place that knows every
// adapter and wires them to the core's ports.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/engine"
	"github.com/johnnycube/stellwerk/internal/adapters/in/chatops"
	httpin "github.com/johnnycube/stellwerk/internal/adapters/in/http"
	"github.com/johnnycube/stellwerk/internal/adapters/in/scheduler"
	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
	"github.com/johnnycube/stellwerk/internal/adapters/out/audit"
	"github.com/johnnycube/stellwerk/internal/adapters/out/coordination/local"
	"github.com/johnnycube/stellwerk/internal/adapters/out/metrics"
	"github.com/johnnycube/stellwerk/internal/adapters/out/silences"
	"github.com/johnnycube/stellwerk/internal/adapters/out/spool"
	"github.com/johnnycube/stellwerk/internal/adapters/out/stats"
	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/config"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/usecase"
	"go.yaml.in/yaml/v3"
)

// haFactory is set by ha_peers.go. Built with -tags noha it stays nil and
// the binary contains no cluster code at all.
var haFactory func(cfg *config.Config, log *slog.Logger) (ports.Coordinator, http.Handler)

// HAAvailable reports whether this binary was built with HA support.
func HAAvailable() bool { return haFactory != nil }

type App struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Spec     domain.Spec
	Sources  httpin.Sources
	Renderer *domain.Renderer
	Router   *domain.Router
	Coord    ports.Coordinator
	Stats    *stats.Memory
	Metrics  *metrics.Metrics
	Audit    *audit.Memory // nil when audit.enabled is false
	Spool    *spool.SQLite // nil when delivery.spool is empty, opened in Connect
	Version  string

	Clock ports.Clock

	// set by Connect / Wire
	Endpoints map[string]target.Target // "target/account" -> sender

	// use cases
	Deliver    *usecase.DeliverMessages
	Ingest     *usecase.IngestEvents
	Simulate   *usecase.SimulateEvents
	Describe   *usecase.DescribeTopology
	Heartbeats *usecase.CheckHeartbeats
	ReadAudit  *usecase.ReadAudit
	Silences   *usecase.ManageSilences
	Digests    *usecase.DigestMessages
	Resend     *usecase.ResendAudit // nil without audit log
	Canary     *usecase.RunCanary   // nil without canary.to

	// Load reads the configuration again from where it came from, for
	// reloads (SIGHUP, file watch, UI). Set by cmd.
	Load func() (*config.Config, error)

	// driving adapters
	HTTP      *httpin.Server
	Scheduler *scheduler.Heartbeats
	Canaries  *scheduler.Canary // nil without canary.to
	Chat      *chatops.Handler
	SelfPing  *scheduler.SelfPing // nil without self_ping.url

	coordHandler http.Handler
	draining     atomic.Bool
	auditFile    *audit.File
	reloadMu     sync.Mutex
	problemsMu   sync.Mutex
	problems     map[string][]string // endpoint -> what keeps delivery from working
	pingTokens   map[string]string
	current      atomic.Pointer[config.Config] // after a reload
}

// config returns the configuration in force, the reloaded one if any.
func (a *App) config() *config.Config {
	if c := a.current.Load(); c != nil {
		return c
	}
	return a.Cfg
}

// Build checks everything that can be checked offline: decoders, templates,
// rules, CEL and HA settings. No network, no logins.
func Build(cfg *config.Config, log *slog.Logger) (*App, error) {
	a := &App{Cfg: cfg, Log: log, Stats: stats.NewMemory(200), Metrics: metrics.New(), Version: "dev", Clock: ports.SystemClock{}}
	known := target.Types()
	for name, t := range cfg.Targets {
		if !contains(known, t.Type) {
			return nil, fmt.Errorf("targets.%s: unknown type %q (known: %v)", name, t.Type, known)
		}
	}
	r, err := compile(cfg)
	if err != nil {
		return nil, err
	}
	a.Spec, a.Sources, a.Renderer, a.Router, a.pingTokens = r.spec, r.sources, r.renderer, r.router, r.pings
	if c := cfg.Audit; c.On() {
		a.Audit = audit.NewMemory(c.Size) // sinks are opened in Connect, -check stays side-effect free
	}
	if cfg.HA.Enabled {
		if haFactory == nil {
			return nil, errors.New("ha.enabled is set but this binary was built without HA support (-tags noha)")
		}
		a.Coord, a.coordHandler = haFactory(cfg, log)
	} else {
		a.Coord = local.New(cfg.NodeID)
	}
	return a, nil
}

// Connect creates one sender per target account (logs in to Matrix etc.)
// and wires the services.
func (a *App) Connect(ctx context.Context) error {
	if c := a.Cfg.Audit; a.Audit != nil {
		if c.File != "" {
			f, err := audit.NewFile(c.File, int64(c.FileMaxMB)<<20, a.Log)
			if err != nil {
				return err
			}
			a.auditFile = f
			a.Audit.AddSink(f)
		}
		if c.Log {
			a.Audit.AddSink(audit.Log{L: a.Log})
		}
	}
	if err := a.openSpool(); err != nil {
		return err
	}
	a.Endpoints = map[string]target.Target{}
	senders := map[string]ports.Sender{}
	names := make([]string, 0, len(a.Cfg.Targets))
	for n := range a.Cfg.Targets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		tc := a.Cfg.Targets[name]
		for _, acc := range tc.AccountNames() {
			own := tc.Accounts[acc]
			settings, err := target.MergeSettings(&tc.Settings, &own)
			if err != nil {
				return fmt.Errorf("targets.%s.accounts.%s: %w", name, acc, err)
			}
			t, err := target.New(ctx, tc.Type, target.Env{Target: name, Account: acc, NodeID: a.Cfg.NodeID, Log: a.Log, Commands: a.chatCommand}, settings)
			if err != nil {
				return fmt.Errorf("targets.%s.accounts.%s: %w", name, acc, err)
			}
			ep := domain.EndpointID(name, acc)
			a.Endpoints[ep] = t
			senders[ep] = t
		}
	}
	a.Wire(senders)
	return nil
}

// compiled is everything a reload can replace: sources, rules, templates,
// destinations and heartbeats.
type compiled struct {
	spec     domain.Spec
	sources  httpin.Sources
	pings    map[string]string
	renderer *domain.Renderer
	router   *domain.Router
}

func compile(cfg *config.Config) (*compiled, error) {
	c := &compiled{spec: cfg.Spec(), sources: httpin.Sources{}, pings: map[string]string{}}
	for _, h := range cfg.Heartbeats {
		if h.Token != "" {
			c.pings[h.Name] = h.Token
		}
	}
	for name, s := range cfg.Sources {
		opts := s.Options
		d, err := source.New(s.Kind, &opts)
		if err != nil {
			return nil, fmt.Errorf("sources.%s: %w", name, err)
		}
		src := httpin.Source{Kind: s.Kind, Token: s.Token, TokenHeader: s.TokenHeader, Decoder: d}
		if s.Signature != nil {
			sig, _ := s.Signature.Resolved() // validated by config
			src.Signature = &httpin.Signature{Header: sig.Header, Prefix: sig.Prefix, Secret: sig.Secret}
		}
		if rate, burst, ok, _ := s.Rate(); ok { // validated by config
			src.RateLimit = &httpin.RateLimit{Rate: rate, Burst: burst}
		}
		c.sources[name] = src
	}
	var err error
	if c.renderer, err = domain.NewRenderer(c.spec.Templates); err != nil {
		return nil, err
	}
	engines, err := domain.NewEngines(cfg.Engine, engine.All()...)
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	if c.router, err = domain.NewRouter(c.spec.Rules, c.spec.Destinations, c.renderer, engines); err != nil {
		return nil, err
	}
	return c, nil
}

// restartOnly lists the config sections a reload cannot change: they hold
// connections, listeners, queues or cluster membership.
func restartOnly(c *config.Config) map[string]string {
	out := map[string]string{}
	for name, v := range map[string]any{
		"listen": c.Listen, "node_id": c.NodeID, "targets": c.Targets, "delivery": c.Delivery,
		"ha": c.HA, "ui": c.UI, "log": c.Log, "metrics": c.Metrics, "audit": c.Audit,
	} {
		b, _ := yaml.Marshal(v)
		out[name] = string(b)
	}
	return out
}

// Reload switches to cfg without a restart: sources, rules, templates,
// destinations (on existing accounts) and heartbeats. Anything else must
// stay the same, otherwise nothing changes and the error says what needs
// a restart. Messages in flight finish with the configuration they started
// with.
func (a *App) Reload(cfg *config.Config) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	was, now := restartOnly(a.config()), restartOnly(cfg)
	var changed []string
	for k := range was {
		if was[k] != now[k] {
			changed = append(changed, k)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		return fmt.Errorf("these sections changed and need a restart, nothing was reloaded: %s", strings.Join(changed, ", "))
	}
	r, err := compile(cfg)
	if err != nil {
		return fmt.Errorf("new config is invalid, keeping the old one: %w", err)
	}
	for alias, d := range r.spec.Destinations {
		if !a.Deliver.HasEndpoint(d.Endpoint()) {
			return fmt.Errorf("destination %s uses %s, which is not a running account", alias, d.Endpoint())
		}
	}
	a.Ingest.SetRouting(r.router, r.spec.Heartbeats)
	a.Simulate.SetRouting(r.router, r.spec.Heartbeats)
	a.Heartbeats.SetRouting(r.spec.Heartbeats, r.router)
	a.Describe.SetSpec(r.spec, r.renderer)
	if a.Resend != nil {
		a.Resend.SetRouter(r.router)
	}
	if a.Canary != nil {
		a.Canary.SetRouter(r.router)
	}
	a.HTTP.SetSources(r.sources)
	a.HTTP.SetPingTokens(r.pings)
	a.current.Store(cfg)
	a.Log.Info("config reloaded", "rules", len(r.spec.Rules), "destinations", len(r.spec.Destinations), "heartbeats", len(r.spec.Heartbeats))
	return nil
}

// ReloadFromSource loads the configuration again and applies it.
func (a *App) ReloadFromSource() error {
	if a.Load == nil {
		return errors.New("this instance cannot reload its config")
	}
	cfg, err := a.Load()
	if err != nil {
		return fmt.Errorf("new config is invalid, keeping the old one: %w", err)
	}
	return a.Reload(cfg)
}

func (a *App) openSpool() error {
	p := a.Cfg.Delivery.Spool
	if p == "" || a.Spool != nil {
		return nil
	}
	s, err := spool.Open(p)
	if err != nil {
		return err
	}
	a.Spool = s
	if n := s.Len(); n > 0 {
		a.Log.Info("spool has deliveries from before the restart", "path", p, "count", n)
	}
	return nil
}

// Wire builds the use cases and the driving adapters around the given
// senders. Tests call it with fakes instead of Connect.
func (a *App) Wire(senders map[string]ports.Sender) {
	s := a.Spec
	rec := stats.Fanout{a.Stats, a.Metrics}
	a.Deliver = usecase.NewDeliverMessages(senders, s.Delivery, rec, a.Log)
	if a.Spool != nil {
		a.Deliver.WithOutbox(a.Spool)
	}
	a.Digests = usecase.NewDigestMessages(a.Deliver, a.Clock, a.Log)
	var silenceStore ports.SilenceStore = silences.NewMemory()
	if a.Spool != nil {
		silenceStore = a.Spool.Silences()
	}
	a.Silences = usecase.NewManageSilences(silenceStore, a.Clock, a.Log)
	a.Ingest = usecase.NewIngestEvents(a.Router, s.Heartbeats, a.Digests, a.Coord, rec, a.Clock, s.Delivery, a.Log).WithSilences(a.Silences)
	a.Simulate = usecase.NewSimulateEvents(a.Router, s.Heartbeats).WithSilences(a.Silences)
	a.Heartbeats = usecase.NewCheckHeartbeats(s.Heartbeats, a.Coord, a.Router, a.Deliver, a.Clock, a.Log).WithSilences(a.Silences)
	a.Describe = usecase.NewDescribeTopology(s, a.Renderer, a.Heartbeats, a.Coord, a.Stats).WithSilences(a.Silences)
	var readAudit ports.ReadAudit
	if a.Audit != nil {
		policy := usecase.AuditPolicy{Payload: a.Cfg.Audit.KeepPayload(), PayloadMax: a.Cfg.Audit.PayloadMax}
		a.Deliver.WithAudit(a.Audit, a.Clock, a.Cfg.NodeID)
		a.Ingest.WithAudit(a.Audit, policy)
		a.Heartbeats.WithAudit(a.Audit, policy)
		a.ReadAudit = usecase.NewReadAudit(a.Audit)
		readAudit = a.ReadAudit
		a.Digests.WithAudit(a.Audit)
		a.Resend = usecase.NewResendAudit(a.Audit, a.Deliver, a.Router, a.Clock, a.Cfg.NodeID)
	}
	var resend ports.ResendAudit
	if a.Resend != nil {
		resend = a.Resend
	}

	a.HTTP = &httpin.Server{
		Ingest: a.Ingest, Simulate: a.Simulate, Describe: a.Describe, Audit: readAudit, Sources: a.Sources, Ready: a.Ready,
		Silences: a.Silences, Resend: resend, Reload: a.ReloadFromSource, Pings: a.Heartbeats, PingTokens: a.pingTokens,
		Targets: a.targetViews, UIPassword: a.Cfg.UI.Password, Mounts: map[string]http.Handler{},
		OnRateLimited: a.Metrics.RateLimited,
	}
	a.Scheduler = &scheduler.Heartbeats{UseCase: a.Heartbeats, Interval: 15 * time.Second, Log: a.Log}
	if c := a.Cfg.Canary; len(c.To) > 0 {
		var rx ports.Receiver
		if c.VerifyWith != "" {
			ep := c.VerifyWith
			if !strings.Contains(ep, "/") {
				ep = domain.EndpointID(ep, a.Cfg.Targets[ep].AccountNames()[0])
			}
			if r, ok := a.Endpoints[ep].(ports.Receiver); ok {
				rx = r
			} else if a.Endpoints != nil {
				a.Log.Warn("canary.verify_with cannot read messages back, only delivery is checked", "endpoint", ep)
			}
		}
		a.Canary = usecase.NewRunCanary(usecase.CanarySpec{To: c.To, Alert: c.Alert, Timeout: c.Timeout}, a.Router, a.Deliver, rx, a.Clock, a.Log)
		if a.Audit != nil {
			a.Canary.WithAudit(a.Audit, a.Cfg.NodeID)
		}
		a.Canaries = &scheduler.Canary{UseCase: a.Canary, Every: c.Every, At: c.At, Log: a.Log}
		a.HTTP.Canary = a.Canary
	}
	a.Chat = &chatops.Handler{Silences: a.Silences, Describe: a.Describe, Canary: a.canaryPort(), Leader: a.Coord.IsLeader}
	if p := a.Cfg.SelfPing; p.URL != "" {
		a.SelfPing = &scheduler.SelfPing{URL: p.URL, FailURL: p.FailURL, Every: p.Every, Health: a.health, Log: a.Log}
	}
	a.Metrics.Gauges = a.gauges
	if a.coordHandler != nil {
		a.HTTP.Mounts["/cluster/"] = a.coordHandler
	}
}

func (a *App) Ready() bool {
	if a.draining.Load() {
		return false // shutting down: load balancers should stop sending
	}
	for _, t := range a.Endpoints {
		if r, ok := t.(target.Readier); ok && !r.Ready() {
			return false
		}
	}
	return true
}

// chatCommand is handed to targets in Connect, before Wire creates the
// handler, so it looks the handler up at call time.
func (a *App) chatCommand(ctx context.Context, id, sender, text string) (string, bool) {
	if a.Chat == nil || !chatops.IsCommand(text) {
		return "", false
	}
	return a.Chat.Handle(ctx, chatops.Command{ID: id, Sender: sender, Text: text})
}

// checkEndpoints asks every target that can tell why delivery would not
// work, for the addresses the destinations use.
func (a *App) checkEndpoints(ctx context.Context) {
	addrs := map[string][]string{}
	for _, d := range a.Describe.Describe().Destinations {
		if d.Address != "" {
			addrs[d.Endpoint()] = append(addrs[d.Endpoint()], d.Address)
		}
	}
	out := map[string][]string{}
	for ep, t := range a.Endpoints {
		if hc, ok := t.(target.HealthChecker); ok {
			if p := hc.Check(ctx, addrs[ep]); len(p) > 0 {
				out[ep] = p
				a.Log.Warn("endpoint has problems", "endpoint", ep, "problems", strings.Join(p, "; "))
			}
		}
	}
	a.problemsMu.Lock()
	a.problems = out
	a.problemsMu.Unlock()
}

func (a *App) endpointProblems(ep string) []string {
	a.problemsMu.Lock()
	defer a.problemsMu.Unlock()
	return a.problems[ep]
}

func (a *App) checkEndpointsLoop(ctx context.Context) {
	select { // give the first sync a moment
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Second):
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		a.checkEndpoints(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *App) canaryPort() ports.RunCanary {
	if a.Canary == nil {
		return nil
	}
	return a.Canary
}

// health is what the self ping reports: fine, or what keeps messages from
// going out.
func (a *App) health() (bool, string) {
	var problems []string
	for _, t := range a.targetViews() {
		ep := domain.EndpointID(t.Target, t.Account)
		if !t.Ready {
			problems = append(problems, ep+" is not ready")
		}
		for _, p := range t.Problems {
			problems = append(problems, ep+": "+p)
		}
	}
	now := a.Clock.Now()
	for ep, b := range a.Deliver.Breakers() {
		if b.OpenUntil.After(now) {
			problems = append(problems, ep+" keeps failing")
		}
	}
	sort.Strings(problems)
	return len(problems) == 0, strings.Join(problems, "\n")
}

func (a *App) targetViews() []httpin.TargetView {
	var out []httpin.TargetView
	for name, tc := range a.Cfg.Targets {
		for _, acc := range tc.AccountNames() {
			ready := true
			if r, ok := a.Endpoints[domain.EndpointID(name, acc)].(target.Readier); ok {
				ready = r.Ready()
			}
			out = append(out, httpin.TargetView{Target: name, Account: acc, Type: tc.Type, Ready: ready, Problems: a.endpointProblems(domain.EndpointID(name, acc))})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return domain.EndpointID(out[i].Target, out[i].Account) < domain.EndpointID(out[j].Target, out[j].Account)
	})
	return out
}

// Run starts background loops and both listeners, blocking until ctx ends.
//
// Shutdown order matters for at-least-once delivery: readiness turns false,
// the listeners stop taking requests and let running ones finish, queued
// deliveries are drained, and only then do targets (Matrix sync) stop.
func (a *App) Run(ctx context.Context) error {
	work, stopWork := context.WithCancel(context.Background())
	defer stopWork()
	for _, t := range a.Endpoints {
		if r, ok := t.(target.Runner); ok {
			go r.Run(work)
		}
	}
	go a.Deliver.Run(work)
	go a.Digests.Run(work)
	go a.Coord.Run(work)
	go a.Scheduler.Run(work)
	if a.Canaries != nil {
		go a.Canaries.Run(work)
	}
	go a.checkEndpointsLoop(work)
	if a.SelfPing != nil {
		go a.SelfPing.Run(work)
	}

	servers := []*http.Server{{Addr: a.Cfg.Listen, Handler: a.HTTP.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}}
	if l := *a.Cfg.UI.Listen; l != "" {
		servers = append(servers, &http.Server{Addr: l, Handler: a.HTTP.UIHandler(), ReadHeaderTimeout: 10 * time.Second})
	}
	if l := a.Cfg.Metrics.Listen; l != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", a.Metrics.Handler())
		servers = append(servers, &http.Server{Addr: l, Handler: mux, ReadHeaderTimeout: 10 * time.Second})
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) {
			a.Log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(s)
	}
	a.Log.Info("started", "ha", a.Cfg.HA.Enabled, "endpoints", len(a.Endpoints), "rules", len(a.Spec.Rules))
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}
	a.shutdown(servers, stopWork)
	return runErr
}

func (a *App) shutdown(servers []*http.Server, stopWork context.CancelFunc) {
	a.draining.Store(true)
	timeout := a.Cfg.Delivery.DrainTimeout
	a.Log.Info("shutting down", "pending", a.Deliver.Pending(), "drain_timeout", timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(ctx) // running requests finish, sync ones wait for their deliveries
	}
	if n := a.Digests.Waiting(); n > 0 {
		a.Log.Info("sending open digests early", "messages", n)
		a.Digests.FlushAll()
	}
	if err := a.Deliver.Drain(ctx); err != nil {
		a.Log.Warn("shutdown before all deliveries finished", "err", err)
	}
	stopWork() // unfinished deliveries stop and stay in the spool
	time.Sleep(50 * time.Millisecond)
	for _, t := range a.Endpoints {
		if c, ok := t.(target.Closer); ok {
			_ = c.Close()
		}
	}
	if a.auditFile != nil {
		_ = a.auditFile.Close()
	}
	if a.Spool != nil {
		if n := a.Spool.Len(); n > 0 {
			a.Log.Info("deliveries kept in the spool for the next start", "count", n)
		}
		_ = a.Spool.Close()
	}
	a.Log.Info("stopped")
}

// gauges are read at every scrape.
func (a *App) gauges() []metrics.Gauge {
	g := []metrics.Gauge{
		{Name: "stellwerk_build_info", Help: "Version and node, always 1.", Labels: map[string]string{"version": a.Version, "node": a.Cfg.NodeID}, Value: 1},
		{Name: "stellwerk_deliveries_pending", Help: "Deliveries queued or being sent.", Value: float64(a.Deliver.Pending())},
		{Name: "stellwerk_leader", Help: "1 if this node raises heartbeat alerts.", Value: metrics.Bool(a.Coord.IsLeader())},
		{Name: "stellwerk_ready", Help: "1 if this node accepts work (readyz).", Value: metrics.Bool(a.Ready())},
	}
	for _, h := range a.Heartbeats.States() {
		l := map[string]string{"heartbeat": h.Name, "source": h.Source}
		g = append(g, metrics.Gauge{Name: "stellwerk_heartbeat_missing", Help: "1 if the heartbeat is overdue.", Labels: l, Value: metrics.Bool(h.Missing)})
		if !h.LastSeen.IsZero() {
			g = append(g, metrics.Gauge{Name: "stellwerk_heartbeat_last_seen_seconds", Help: "Unix time of the last heartbeat.", Labels: l, Value: float64(h.LastSeen.Unix())})
		}
	}
	for _, t := range a.targetViews() {
		g = append(g, metrics.Gauge{
			Name: "stellwerk_endpoint_ready", Help: "1 if a target account can deliver.",
			Labels: map[string]string{"target": t.Target, "account": t.Account, "type": t.Type}, Value: metrics.Bool(t.Ready),
		},
			metrics.Gauge{
				Name: "stellwerk_endpoint_problems", Help: "Known problems of a target account (not in a room, room not encrypted, device not verified).",
				Labels: map[string]string{"target": t.Target, "account": t.Account, "type": t.Type}, Value: float64(len(t.Problems)),
			})
	}
	now := a.Clock.Now()
	for ep, b := range a.Deliver.Breakers() {
		g = append(g, metrics.Gauge{
			Name: "stellwerk_endpoint_breaker_open", Help: "1 while an account is skipped after failing too often in a row.",
			Labels: map[string]string{"endpoint": ep}, Value: metrics.Bool(b.OpenUntil.After(now)),
		})
	}
	g = append(g, metrics.Gauge{Name: "stellwerk_digest_waiting", Help: "Messages collected for a digest, not sent yet.", Value: float64(a.Digests.Waiting())},
		metrics.Gauge{Name: "stellwerk_silences_active", Help: "Silences in force.", Value: float64(len(a.Silences.Active()))})
	if a.Canary != nil {
		if last, ok := a.Canary.Last(); ok {
			g = append(g, metrics.Gauge{Name: "stellwerk_canary_ok", Help: "1 if the last canary arrived.", Value: metrics.Bool(last.OK)},
				metrics.Gauge{Name: "stellwerk_canary_last_run_seconds", Help: "Unix time of the last canary.", Value: float64(last.Time.Unix())})
		}
	}
	if a.Spool != nil {
		g = append(g, metrics.Gauge{Name: "stellwerk_spool_deliveries", Help: "Deliveries in the durable spool, not finished yet.", Value: float64(a.Spool.Len())})
	}
	if a.Audit != nil {
		g = append(g, metrics.Gauge{Name: "stellwerk_audit_entries", Help: "Audit entries kept in memory.", Value: float64(a.Audit.Len())},
			metrics.Gauge{Name: "stellwerk_audit_evicted", Help: "Audit entries pushed out of memory since start.", Value: float64(a.Audit.Evicted())})
	}
	if st := a.Coord.Status(); st.Enabled {
		for _, p := range st.Peers {
			g = append(g, metrics.Gauge{Name: "stellwerk_peer_alive", Help: "1 if an HA peer answers.", Labels: map[string]string{"peer": p.ID}, Value: metrics.Bool(p.Alive)})
		}
	}
	return g
}

// BootstrapCrossSigning runs the one-time identity setup for an endpoint
// ("matrix/alerts", or just "matrix" for a single-account target).
func (a *App) BootstrapCrossSigning(ctx context.Context, endpoint string) (string, error) {
	if _, ok := a.Endpoints[endpoint]; !ok {
		if tc, found := a.Cfg.Targets[endpoint]; found && len(tc.AccountNames()) == 1 {
			endpoint = domain.EndpointID(endpoint, tc.AccountNames()[0])
		}
	}
	t, ok := a.Endpoints[endpoint].(interface {
		BootstrapCrossSigning(context.Context) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("endpoint %q does not exist or does not support cross-signing (endpoints: %v)", endpoint, a.endpointNames())
	}
	return t.BootstrapCrossSigning(ctx)
}

func (a *App) endpointNames() []string {
	var out []string
	for k := range a.Endpoints {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
