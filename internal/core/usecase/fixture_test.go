package usecase

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

// world is a complete core with fakes on every driven port.
type world struct {
	t       *testing.T
	ctx     context.Context
	clock   *portstest.Clock
	coord   *portstest.Coordinator
	rec     *portstest.Recorder
	alerts  *portstest.Sender // matrix/alerts
	media   *portstest.Sender // matrix/media
	hook    *portstest.Sender // hook/default
	audit   *portstest.AuditStore
	spec    domain.Spec
	router  *domain.Router
	rend    *domain.Renderer
	deliver *DeliverMessages
	ingest  *IngestEvents
	hb      *CheckHeartbeats
	sim     *SimulateEvents
	topo    *DescribeTopology
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testSpec() domain.Spec {
	return domain.Spec{
		Destinations: map[string]domain.Destination{
			"critical": {Alias: "critical", Target: "matrix", Account: "alerts", Address: "!crit:x"},
			"media":    {Alias: "media", Target: "matrix", Account: "media", Address: "!media:x"},
			"pager":    {Alias: "pager", Target: "hook", Account: "default", Address: "/alerts"},
		},
		Rules: []domain.Rule{
			{Name: "critical", Match: domain.Match{Source: "alertmanager", Labels: map[string]string{"severity": "critical"}},
				To: []string{"critical", "pager"}, Loud: true, Mention: []string{"@me:x"}},
			{Name: "noise", Match: domain.Match{Source: "k8s", Fields: map[string]string{"type": "Normal"}}, Drop: true},
			{Name: "media", Match: domain.Match{Source: "*arr"}, To: []string{"media"}},
			{Name: "broken", Match: domain.Match{Source: "broken"}, To: []string{"critical"}, Template: "bad"},
			{Name: "rest", Match: domain.Match{Source: "x*"}, To: []string{"critical"}},
		},
		Heartbeats: []domain.Heartbeat{
			{Name: "wd", Source: "alertmanager", Labels: map[string]string{"alertname": "Watchdog"}, Timeout: 10 * time.Minute,
				To: []string{"critical", "pager"}, Mention: []string{"@me:x"}},
		},
		Templates: map[string]string{
			"bad":    `{{ index .Labels 5 }}`, // fails at render time
			"custom": `custom {{ .Source }}`,
		},
		Delivery: domain.DeliveryPolicy{Sync: true, Timeout: 2 * time.Second, QueueSize: 10, MaxRetries: 2, MaxBackoff: time.Millisecond, DedupeWindow: time.Minute},
	}
}

func newWorld(t *testing.T, mutate ...func(*domain.Spec)) *world {
	t.Helper()
	w := &world{t: t, clock: portstest.NewClock(), rec: portstest.NewRecorder(),
		alerts: portstest.NewSender(), media: portstest.NewSender(), hook: portstest.NewSender(), spec: testSpec(),
		audit: portstest.NewAuditStore()}
	for _, m := range mutate {
		m(&w.spec)
	}
	w.coord = portstest.NewCoordinator("node-a", w.clock)
	var err error
	if w.rend, err = domain.NewRenderer(w.spec.Templates); err != nil {
		t.Fatal(err)
	}
	if w.router, err = domain.NewRouter(w.spec.Rules, w.spec.Destinations, w.rend, domain.Engines{}); err != nil {
		t.Fatal(err)
	}
	senders := map[string]ports.Sender{"matrix/alerts": w.alerts, "matrix/media": w.media, "hook/default": w.hook}
	policy := AuditPolicy{Payload: true, PayloadMax: 64}
	w.deliver = NewDeliverMessages(senders, w.spec.Delivery, w.rec, quiet()).WithAudit(w.audit, w.clock, "node-a")
	w.ingest = NewIngestEvents(w.router, w.spec.Heartbeats, w.deliver, w.coord, w.rec, w.clock, w.spec.Delivery, quiet()).WithAudit(w.audit, policy)
	w.hb = NewCheckHeartbeats(w.spec.Heartbeats, w.coord, w.router, w.deliver, w.clock, quiet()).WithAudit(w.audit, policy)
	w.sim = NewSimulateEvents(w.router, w.spec.Heartbeats)
	w.topo = NewDescribeTopology(w.spec, w.rend, w.hb, w.coord, w.rec)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx
	go w.deliver.Run(ctx)
	return w
}

func critical() *domain.Event {
	return &domain.Event{Source: "alertmanager", Kind: "alertmanager", Status: "firing",
		Labels: map[string]string{"alertname": "Disk", "severity": "critical"},
		Data:   map[string]any{"alerts": []any{map[string]any{"status": "firing", "annotations": map[string]any{"summary": "Disk full"}}}}}
}

func watchdog() *domain.Event {
	return &domain.Event{Source: "alertmanager", Kind: "alertmanager", Status: "firing", Labels: map[string]string{"alertname": "Watchdog"}}
}

func generic(source string, data map[string]any) *domain.Event {
	return &domain.Event{Source: source, Kind: "generic", Data: data}
}

// last returns the newest audit entry.
func (w *world) last() ports.AuditEntry {
	all := w.audit.All()
	if len(all) == 0 {
		w.t.Fatal("no audit entry")
	}
	return all[len(all)-1]
}

func (w *world) in(raw string, evs ...*domain.Event) ports.IngestResult {
	return w.ingest.Ingest(w.ctx, ports.IngestRequest{Events: evs, Raw: []byte(raw)})
}

// eventually waits for an asynchronous condition.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
