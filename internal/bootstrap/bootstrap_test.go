package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/johnnycube/stellwerk/internal/adapters/engine/all"
	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	_ "github.com/johnnycube/stellwerk/internal/adapters/out/target/all"
	"github.com/johnnycube/stellwerk/internal/config"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
	"github.com/johnnycube/stellwerk/internal/core/usecase"
)

type fakeSender struct {
	mu   sync.Mutex
	sent []sent
	fail bool
}

type sent struct {
	Address string
	Msg     domain.Message
}

func (f *fakeSender) Send(_ context.Context, addr string, m domain.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("target down")
	}
	f.sent = append(f.sent, sent{addr, m})
	return nil
}

func (f *fakeSender) list() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

const testConfig = `
node_id: %NODE%
sources:
  alertmanager: { kind: alertmanager, token: am }
  sonarr: { kind: generic, token: so }
  "*": { kind: generic, token: def }
targets:
  matrix:
    type: matrix
    settings: { homeserver: x }
    accounts:
      alerts: { user_id: "@alerts:x" }
      media: { user_id: "@media:x" }
  hook: { type: webhook, settings: { url: http://x } }
destinations:
  critical: { target: matrix, account: alerts, address: "!crit:x" }
  media: { target: matrix, account: media, address: "!media:x" }
  misc: { target: matrix, account: alerts, address: "!misc:x" }
  pager: { target: hook }
routes:
  - name: critical
    match: { source: alertmanager, labels: { severity: critical } }
    to: [critical, pager]
    loud: true
  - name: media
    match: { source: "*arr" }
    expr: 'data.?eventType.orValue("") != "Test"'
    to: media
  - name: arr-noise
    match: { source: "*arr" }
    drop: true
  - name: rest
    to: misc
heartbeats:
  - { name: wd, source: alertmanager, labels: { alertname: Watchdog }, timeout: 1m, to: critical }
delivery: { mode: %MODE%, timeout: 2s, max_retries: 2, max_backoff: 1ms, dedupe_window: 1m }
%CLUSTER%
`

type node struct {
	app    *App
	matrix *fakeSender // matrix/alerts
	media  *fakeSender // matrix/media
	hook   *fakeSender
	h      http.Handler
}

func newNode(ctx context.Context, t *testing.T, id, mode, cluster string) *node {
	t.Helper()
	y := strings.NewReplacer("%NODE%", id, "%MODE%", mode, "%CLUSTER%", cluster).Replace(testConfig)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	n := &node{app: a, matrix: &fakeSender{}, media: &fakeSender{}, hook: &fakeSender{}}
	a.Wire(map[string]ports.Sender{"matrix/alerts": n.matrix, "matrix/media": n.media, "hook/default": n.hook})
	go a.Deliver.Run(ctx)
	go a.Coord.Run(ctx)
	n.h = a.HTTP.Handler()
	return n
}

func post(h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const amCritical = `{"version":"4","status":"firing","receiver":"r",
 "groupLabels":{"alertname":"Disk"},"commonLabels":{"alertname":"Disk","severity":"critical"},
 "alerts":[{"status":"firing","labels":{"alertname":"Disk"},"annotations":{"summary":"Disk full"}}]}`

func TestSyncDeliveryToTwoTargets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	rec := post(n.h, "/in/alertmanager", "am", amCritical)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	m, h := n.matrix.list(), n.hook.list()
	if len(m) != 1 || m[0].Address != "!crit:x" || !m[0].Msg.Loud || !strings.Contains(m[0].Msg.Markdown, "Disk full") {
		t.Errorf("matrix: %+v", m)
	}
	if len(h) != 1 || h[0].Msg.Route != "critical" {
		t.Errorf("hook: %+v", h)
	}
	ev := n.app.Stats.Snapshot().Events[0]
	if ev.Routes[0] != "critical" {
		t.Errorf("routes: %v", ev.Routes)
	}
	if ev.Verdict != "delivered" || ev.Node != "a" {
		t.Errorf("stats: %+v", ev)
	}
}

func TestSyncFailureMakesSenderRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	n.hook.fail = true
	rec := post(n.h, "/in/alertmanager", "am", amCritical)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "target down") {
		t.Fatalf("want 502, got %d %s", rec.Code, rec.Body)
	}
	// failed deliveries must not be marked as seen, the retry has to go through
	n.hook.fail = false
	if rec := post(n.h, "/in/alertmanager", "am", amCritical); rec.Code != http.StatusOK {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
	// now the same payload is a duplicate
	if rec := post(n.h, "/in/alertmanager", "am", amCritical); rec.Code != http.StatusNoContent {
		t.Errorf("duplicate: %d", rec.Code)
	}
	if got := n.app.Stats.Snapshot().Events[0].Verdict; got != "duplicate" {
		t.Errorf("verdict %s", got)
	}
}

func TestAsyncAndRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "async", "")
	if rec := post(n.h, "/in/sonarr", "so", `{"eventType":"Download","series":{"title":"S"}}`); rec.Code != http.StatusAccepted {
		t.Fatalf("async: %d", rec.Code)
	}
	if rec := post(n.h, "/in/sonarr", "so", `{"eventType":"Test"}`); rec.Code != http.StatusNoContent {
		t.Errorf("drop: %d", rec.Code)
	}
	if rec := post(n.h, "/in/radarr", "def", `{"eventType":"Download"}`); rec.Code != http.StatusAccepted {
		t.Errorf("fallback source: %d", rec.Code)
	}
	if rec := post(n.h, "/in/backup?host=nas", "def", "done"); rec.Code != http.StatusAccepted {
		t.Errorf("plain text: %d", rec.Code)
	}
	time.Sleep(100 * time.Millisecond)
	// media goes through the media account, the rest through alerts
	if got := n.media.list(); len(got) != 2 || got[0].Address != "!media:x" || got[0].Msg.Loud {
		t.Errorf("media account: %+v", got)
	}
	if got := n.matrix.list(); len(got) != 1 || got[0].Address != "!misc:x" {
		t.Errorf("alerts account: %+v", got)
	}
}

func TestAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	cases := []struct {
		path, token string
		code        int
	}{
		{"/in/alertmanager", "", 401},
		{"/in/alertmanager", "def", 401}, // own token required, no fallback
		{"/in/sonarr", "am", 401},
		{"/in/whatever?token=def", "", 200},
		{"/in/bad%20name", "def", 400},
	}
	for _, c := range cases {
		if rec := post(n.h, c.path, c.token, `{"x":1}`); rec.Code != c.code {
			t.Errorf("%s token=%q: got %d want %d", c.path, c.token, rec.Code, c.code)
		}
	}
	// garbage for a typed decoder
	if rec := post(n.h, "/in/alertmanager", "am", `{"nope":1}`); rec.Code != 400 {
		t.Errorf("decoder error: %d", rec.Code)
	}
}

func TestHeartbeatConsumed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	rec := post(n.h, "/in/alertmanager", "am", `{"status":"firing","commonLabels":{"alertname":"Watchdog"},"alerts":[]}`)
	if rec.Code != http.StatusNoContent || len(n.matrix.list()) != 0 {
		t.Fatalf("heartbeat: %d", rec.Code)
	}
	if _, ok := n.app.Coord.Heartbeat("wd"); !ok {
		t.Error("heartbeat not stored")
	}
}

// Two nodes behind a "load balancer": state must be shared.
func TestHighAvailabilityTwoNodes(t *testing.T) {
	if !HAAvailable() {
		t.Skip("built with -tags noha")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// servers first, so we know the URLs for the peer list
	var ha, hb http.Handler
	var mu sync.RWMutex
	wrap := func(get func() http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.RLock()
			h := get()
			mu.RUnlock()
			if h == nil {
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	sa := httptest.NewServer(wrap(func() http.Handler { return ha }))
	sb := httptest.NewServer(wrap(func() http.Handler { return hb }))
	defer sa.Close()
	defer sb.Close()
	cl := "ha: { enabled: true, secret: s, interval: 50ms, peers: [ { id: a, url: \"" + sa.URL + "\" }, { id: b, url: \"" + sb.URL + "\" } ] }"
	a := newNode(ctx, t, "a", "sync", cl)
	b := newNode(ctx, t, "b", "sync", cl)
	mu.Lock()
	ha, hb = a.h, b.h
	mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for !a.app.Coord.IsLeader() || b.app.Coord.IsLeader() {
		if time.Now().After(deadline) {
			t.Fatal("no leader agreement")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// heartbeat lands on b, a (the leader) must know it
	post(sb.Config.Handler, "/in/alertmanager", "am", `{"status":"firing","commonLabels":{"alertname":"Watchdog"},"alerts":[]}`)
	for {
		if _, ok := a.app.Coord.Heartbeat("wd"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat not shared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.app.Heartbeats.Check()
	b.app.Heartbeats.Check()
	if len(a.matrix.list())+len(b.matrix.list()) != 0 {
		t.Error("false heartbeat alarm")
	}

	// sender delivers on a, retries the same payload on b: delivered once
	if rec := post(sa.Config.Handler, "/in/alertmanager", "am", amCritical); rec.Code != 200 {
		t.Fatalf("a: %d", rec.Code)
	}
	for !b.app.Coord.Seen(dedupeKeyFor(t, amCritical)) {
		if time.Now().After(deadline) {
			t.Fatal("dedupe key not shared")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rec := post(sb.Config.Handler, "/in/alertmanager", "am", amCritical); rec.Code != http.StatusNoContent {
		t.Errorf("b should see a duplicate, got %d", rec.Code)
	}
	if len(a.matrix.list()) != 1 || len(b.matrix.list()) != 0 {
		t.Errorf("deliveries a=%d b=%d", len(a.matrix.list()), len(b.matrix.list()))
	}

	// status shows both nodes
	st := b.app.Coord.Status()
	if !st.Enabled || st.Leader != "a" || len(st.Peers) != 2 || !st.Peers[0].Alive {
		t.Errorf("status: %+v", st)
	}
}

func dedupeKeyFor(t *testing.T, body string) string {
	d, _ := source.New("alertmanager", nil)
	evs, err := d.Decode(source.Input{Source: "alertmanager", Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return usecase.DedupeKey(evs[0], []byte(body), 0)
}

func TestUIFlowsAndSimulate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	ui := n.app.HTTP.UIHandler()

	rec := httptest.NewRecorder()
	ui.ServeHTTP(rec, httptest.NewRequest("GET", "/api/flows", nil))
	var f struct {
		Routes []struct {
			To []string `json:"to"`
		} `json:"routes"`
		Destinations []struct{ Alias, Account, Type string }  `json:"destinations"`
		Targets      []struct{ Target, Account, Type string } `json:"targets"`
		Kinds        []any                                    `json:"kinds"`
		Heartbeats   []any                                    `json:"heartbeats"`
		Cluster      ports.ClusterStatus                      `json:"cluster"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Routes) != 4 || len(f.Destinations) != 4 || len(f.Targets) != 3 || len(f.Kinds) < 2 || f.Cluster.Enabled || len(f.Heartbeats) != 1 {
		t.Errorf("flows: routes=%d dests=%d targets=%d kinds=%d ha=%v hb=%d", len(f.Routes), len(f.Destinations), len(f.Targets), len(f.Kinds), f.Cluster.Enabled, len(f.Heartbeats))
	}
	if f.Routes[0].To[1] != "pager" || f.Destinations[3].Type != "webhook" || f.Destinations[1].Account != "media" || f.Targets[2].Account != "media" {
		t.Errorf("route/destination view: %+v %+v %+v", f.Routes[0], f.Destinations, f.Targets)
	}

	sim := func(src, body string) simulateResponse {
		b, _ := json.Marshal(map[string]string{"source": src, "body": body})
		rec := httptest.NewRecorder()
		ui.ServeHTTP(rec, httptest.NewRequest("POST", "/api/simulate", strings.NewReader(string(b))))
		var r simulateResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		return r
	}
	r := sim("alertmanager", amCritical)
	if r.Kind != "alertmanager" || len(r.Events) != 1 || len(r.Events[0].Deliveries) != 2 || !strings.Contains(r.Events[0].Deliveries[0].HTML, "<strong>Disk</strong>") {
		t.Fatalf("simulate: %+v", r)
	}
	if len(n.matrix.list()) != 0 {
		t.Error("simulate must not send")
	}
	r = sim("sonarr", `{"eventType":"Test"}`)
	if r.Events[0].DroppedBy != "arr-noise" || r.Events[0].Trace[1].Reason != "expr is false" {
		t.Errorf("drop trace: %+v", r.Events[0])
	}
	r = sim("cron", "<script>x</script>")
	if strings.Contains(r.Events[0].Deliveries[0].HTML, "<script>") {
		t.Error("html not escaped")
	}
	if r = sim("alertmanager", `{"status":"firing","commonLabels":{"alertname":"Watchdog"}}`); r.Events[0].Heartbeat != "wd" {
		t.Errorf("heartbeat sim: %+v", r)
	}
	if _, ok := n.app.Coord.Heartbeat("wd"); ok {
		t.Error("simulate must not touch heartbeat state")
	}
}

type simulateResponse struct {
	Kind   string `json:"kind"`
	Error  string `json:"error"`
	Events []struct {
		Heartbeat  string             `json:"heartbeat"`
		DroppedBy  string             `json:"droppedBy"`
		Trace      []domain.TraceStep `json:"trace"`
		Deliveries []struct {
			Endpoint string `json:"endpoint"`
			HTML     string `json:"html"`
		} `json:"deliveries"`
	} `json:"events"`
}

func TestHADisabledIgnoresPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "ha: { peers: [ { id: a, url: http://127.0.0.1:1 }, { id: b, url: http://127.0.0.1:2 } ] }")
	if st := n.app.Coord.Status(); st.Enabled || !n.app.Coord.IsLeader() {
		t.Errorf("ha should be off: %+v", st)
	}
	rec := httptest.NewRecorder()
	n.h.ServeHTTP(rec, httptest.NewRequest("GET", "/cluster/v1/state", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("cluster API must not exist without ha, got %d", rec.Code)
	}
}

func TestHAEnabledWithoutSupport(t *testing.T) {
	if HAAvailable() {
		t.Skip("built with HA support")
	}
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "ha: { enabled: true, secret: s, peers: [ { id: a, url: http://a }, { id: b, url: http://b } ] }").Replace(testConfig)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "without HA support") {
		t.Errorf("want clear error, got %v", err)
	}
}

// A rolling restart sends SIGTERM. Work accepted before it must still go out.
func TestShutdownDrainsAcceptedWork(t *testing.T) {
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "async", "%CLUSTER%", "listen: \"127.0.0.1:0\"\nui: { listen: \"\" }").Replace(testConfig)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	slow := portstest.NewSender()
	slow.Block()
	a.Wire(map[string]ports.Sender{"matrix/alerts": slow, "matrix/media": portstest.NewSender(), "hook/default": portstest.NewSender()})

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- a.Run(ctx) }()

	ev := &domain.Event{Source: "backup", Kind: "generic", Data: map[string]any{"text": "x"}}
	if res := a.Ingest.Ingest(context.Background(), ports.IngestRequest{Events: []*domain.Event{ev}, Raw: []byte("x")}); res.Outcome != ports.Accepted {
		t.Fatalf("accepted: %v", res.Outcome)
	}
	<-slow.Attempted()
	stop() // SIGTERM while the delivery is still in flight
	time.Sleep(50 * time.Millisecond)
	if a.Ready() {
		t.Error("readiness must turn false while draining")
	}
	select {
	case <-done:
		t.Fatal("Run returned before the delivery finished")
	default:
	}
	slow.Release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if len(slow.Sent()) != 1 {
		t.Errorf("accepted work was lost on shutdown: sent %d", len(slow.Sent()))
	}
}

func TestMetricsCountWhatHappens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	post(n.h, "/in/alertmanager", "am", amCritical)
	post(n.h, "/in/alertmanager", "am", `{"status":"firing","commonLabels":{"alertname":"Watchdog"},"alerts":[]}`)
	var b strings.Builder
	n.app.Metrics.Write(&b)
	out := b.String()
	for _, want := range []string{
		`stellwerk_events_total{source="alertmanager",verdict="delivered"} 1`,
		`stellwerk_events_total{source="alertmanager",verdict="heartbeat"} 1`,
		`stellwerk_deliveries_total{destination="pager",result="ok",route="critical"} 1`,
		`stellwerk_heartbeat_missing{heartbeat="wd",source="alertmanager"} 0`,
		`stellwerk_endpoint_ready{account="media",target="matrix",type="matrix"} 1`,
		`stellwerk_leader 1`,
		`stellwerk_build_info{node="a",version="dev"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestAuditLogThroughTheWholeStack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	n.hook.fail = true // one destination fails, with retries
	post(n.h, "/in/alertmanager", "am", amCritical)
	post(n.h, "/in/alertmanager", "am", `{"nope":1}`) // bad payload
	post(n.h, "/in/sonarr", "so", `{"eventType":"Test"}`)
	ui := n.app.HTTP.UIHandler()
	get := func(path string, into any) {
		rec := httptest.NewRecorder()
		ui.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatal(err)
		}
	}
	var list struct {
		Enabled bool `json:"enabled"`
		Entries []struct {
			ID, Source, Verdict, Reason string
			Rules                       []string
			Destinations                []struct {
				Name, Status string
				Attempts     int
			}
		} `json:"entries"`
	}
	get("/api/audit", &list)
	if !list.Enabled || len(list.Entries) != 3 {
		t.Fatalf("list %+v", list)
	}
	drop, bad, crit := list.Entries[0], list.Entries[1], list.Entries[2]
	if drop.Verdict != "dropped" || bad.Verdict != "rejected" || !strings.Contains(bad.Reason, "invalid alertmanager payload") {
		t.Errorf("drop %+v bad %+v", drop, bad)
	}
	if crit.Verdict != "failed" || len(crit.Rules) != 1 || crit.Destinations[0].Status != "sent" || crit.Destinations[1].Status != "failed" || crit.Destinations[1].Attempts != 2 {
		t.Errorf("critical %+v", crit)
	}
	var filtered struct{ Entries []struct{ ID string } }
	get("/api/audit?verdict=failed", &filtered)
	if len(filtered.Entries) != 1 || filtered.Entries[0].ID != crit.ID {
		t.Errorf("filter %+v", filtered)
	}
	var detail struct {
		Payload    string
		Trace      []domain.TraceStep
		Deliveries []struct {
			Destination, Status, HTML string
			Attempts                  []struct{ Error string }
		}
	}
	get("/api/audit/"+crit.ID, &detail)
	if !strings.Contains(detail.Payload, "Disk full") || len(detail.Trace) != 4 || !strings.Contains(detail.Deliveries[0].HTML, "<strong>Disk</strong>") ||
		detail.Deliveries[1].Attempts[0].Error != "target down" {
		t.Errorf("detail %+v", detail)
	}
	rec := httptest.NewRecorder()
	ui.ServeHTTP(rec, httptest.NewRequest("GET", "/api/audit/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id: %d", rec.Code)
	}
}

func TestAuditCanBeSwitchedOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "audit: { enabled: false }")
	post(n.h, "/in/alertmanager", "am", amCritical)
	if n.app.Audit != nil || n.app.ReadAudit != nil {
		t.Fatal("no audit store when switched off")
	}
	rec := httptest.NewRecorder()
	n.app.HTTP.UIHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/audit", nil))
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("%s", rec.Body)
	}
}

func TestSpoolSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/spool-{node}.db"
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "async",
		"%CLUSTER%", "listen: \"127.0.0.1:0\"\nui: { listen: \"\" }").Replace(testConfig)
	y = strings.Replace(y, "dedupe_window: 1m }", "dedupe_window: 1m, spool: \""+path+"\", drain_timeout: 100ms }", 1)
	start := func(alerts ports.Sender) (*App, chan error, context.CancelFunc) {
		cfg, err := config.Parse([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		a, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.openSpool(); err != nil {
			t.Fatal(err)
		}
		a.Wire(map[string]ports.Sender{"matrix/alerts": alerts, "matrix/media": portstest.NewSender(), "hook/default": portstest.NewSender()})
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Run(ctx) }()
		return a, done, stop
	}

	stuck := portstest.NewSender()
	stuck.Block()
	defer stuck.Release()
	a, done, stop := start(stuck)
	if !strings.HasSuffix(a.Cfg.Delivery.Spool, "spool-a.db") {
		t.Errorf("{node} not replaced: %s", a.Cfg.Delivery.Spool)
	}
	ev := &domain.Event{Source: "backup", Kind: "generic", Data: map[string]any{"text": "keep me"}}
	if res := a.Ingest.Ingest(context.Background(), ports.IngestRequest{Events: []*domain.Event{ev}, Raw: []byte("x")}); res.Outcome != ports.Accepted {
		t.Fatalf("accepted: %v", res.Outcome)
	}
	<-stuck.Attempted()
	stop() // drain times out after 100ms, the delivery stays in the spool
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	fresh := portstest.NewSender()
	b, done2, stop2 := start(fresh)
	defer func() { stop2(); <-done2 }()
	eventually(t, "resumed delivery", func() bool { return len(fresh.Sent()) == 1 })
	if m := fresh.Sent()[0].Msg.Markdown; !strings.Contains(m, "keep me") {
		t.Errorf("message %q", m)
	}
	eventually(t, "spool empty", func() bool { return b.Spool.Len() == 0 })
	var buf strings.Builder
	rec := httptest.NewRecorder()
	b.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	buf.WriteString(rec.Body.String())
	if !strings.Contains(buf.String(), "stellwerk_spool_deliveries") {
		t.Error("spool gauge missing")
	}
}

func TestFallbackAndPriorityFromConfig(t *testing.T) {
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "").Replace(testConfig)
	y = strings.Replace(y, `critical: { target: matrix, account: alerts, address: "!crit:x" }`,
		`critical: { target: matrix, account: alerts, address: "!crit:x", fallback: media }`, 1)
	y = strings.Replace(y, "    loud: true\n", "    loud: true\n    priority: urgent\n", 1)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	down, media := portstest.NewSender(), portstest.NewSender()
	down.FailAll(true)
	a.Wire(map[string]ports.Sender{"matrix/alerts": down, "matrix/media": media, "hook/default": portstest.NewSender()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Deliver.Run(ctx)
	if rec := post(a.HTTP.Handler(), "/in/alertmanager", "am", amCritical); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := media.Sent(); len(got) != 1 || got[0].Msg.Priority != domain.PriorityUrgent || !strings.Contains(got[0].Msg.Markdown, "Fallback") {
		t.Errorf("%+v", got)
	}
	if e := a.Audit.List(ports.AuditQuery{Limit: 1}); len(e) != 1 || e[0].Verdict != ports.AuditFallback {
		t.Errorf("%+v", e)
	}
}

func TestEngineConfig(t *testing.T) {
	base := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "").Replace(testConfig)
	for name, y := range map[string]string{
		"unknown default": "engine: lua\n" + base,
		"unknown on rule": strings.Replace(base, "    to: media\n", "    engine: lua\n    to: media\n", 1),
	} {
		cfg, err := config.Parse([]byte(y))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "lua") {
			t.Errorf("%s: %v", name, err)
		}
	}
	cfg, _ := config.Parse([]byte(strings.Replace(base, "    to: media\n", "    engine: cel\n    to: media\n", 1)))
	if _, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Error(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func uiDo(h http.Handler, method, path, body string, action bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if action {
		req.Header.Set("X-Stellwerk-Action", "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	base := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "").Replace(testConfig)
	load := func(y string) func() (*config.Config, error) {
		return func() (*config.Config, error) { return config.Parse([]byte(y)) }
	}
	// every alert goes to media now, and a new token for sonarr
	next := strings.Replace(base, "to: [critical, pager]", "to: media", 1)
	next = strings.Replace(next, "sonarr: { kind: generic, token: so }", "sonarr: { kind: generic, token: so2 }", 1)
	n.app.Load = load(next)
	if rec := uiDo(n.app.HTTP.UIHandler(), "POST", "/api/reload", "", true); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := post(n.h, "/in/alertmanager", "am", amCritical); rec.Code != 200 || len(n.media.list()) != 1 || len(n.matrix.list()) != 0 {
		t.Errorf("new rules: %d media=%d", rec.Code, len(n.media.list()))
	}
	if post(n.h, "/in/sonarr", "so", `{}`).Code != 401 || post(n.h, "/in/sonarr", "so2", `{"x":1}`).Code != 200 {
		t.Error("new token")
	}
	if rec := uiDo(n.app.HTTP.UIHandler(), "GET", "/api/flows", "", false); !strings.Contains(rec.Body.String(), `"to":["media"]`) {
		t.Error("UI shows the new rules")
	}
	for name, y := range map[string]string{
		"restart":        strings.Replace(next, "max_retries: 2", "max_retries: 3", 1),
		"invalid":        strings.Replace(next, "to: misc", "to: nowhere", 1),
		"bad expression": strings.Replace(next, `expr: 'data.?eventType.orValue("") != "Test"'`, `expr: 'data =='`, 1),
	} {
		n.app.Load = load(y)
		if err := n.app.ReloadFromSource(); err == nil {
			t.Errorf("%s: must be refused", name)
		} else if name == "restart" && !strings.Contains(err.Error(), "delivery") {
			t.Errorf("restart: %v", err)
		}
	}
	before := len(n.media.list())
	if rec := post(n.h, "/in/alertmanager", "am", strings.ReplaceAll(amCritical, "Disk", "Disk2")); rec.Code != 200 || len(n.media.list()) != before+1 {
		t.Error("a refused reload keeps the last good config")
	}
	n.app.Load = nil
	if err := n.app.ReloadFromSource(); err == nil {
		t.Error("no loader")
	}
	if rec := uiDo(n.app.HTTP.UIHandler(), "POST", "/api/reload", "", false); rec.Code != http.StatusForbidden {
		t.Errorf("actions need the header (CSRF): %d", rec.Code)
	}
}

func TestSilencesAndResendOverTheUI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	ui := n.app.HTTP.UIHandler()
	rec := uiDo(ui, "POST", "/api/silences", `{"matchers":"source=alertmanager","for":"1h","comment":"maintenance"}`, true)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var sil domain.Silence
	_ = json.Unmarshal(rec.Body.Bytes(), &sil)
	if post(n.h, "/in/alertmanager", "am", amCritical).Code != http.StatusNoContent || len(n.matrix.list()) != 0 {
		t.Fatal("silenced")
	}
	if rec := uiDo(ui, "GET", "/api/silences", "", false); !strings.Contains(rec.Body.String(), sil.ID) {
		t.Error("list")
	}
	if rec := uiDo(ui, "POST", "/api/silences", `{"matchers":"","for":"1h"}`, true); rec.Code != 400 {
		t.Errorf("bad silence: %d", rec.Code)
	}
	if rec := uiDo(ui, "DELETE", "/api/silences/"+sil.ID, "", true); rec.Code != 204 {
		t.Fatalf("delete %d", rec.Code)
	}
	if rec := uiDo(ui, "DELETE", "/api/silences/nope", "", true); rec.Code != 404 {
		t.Errorf("delete unknown %d", rec.Code)
	}
	silencedEntry := n.app.Audit.List(ports.AuditQuery{Verdict: ports.AuditSilenced})
	if len(silencedEntry) != 1 {
		t.Fatalf("%+v", silencedEntry)
	}

	n.matrix.mu.Lock()
	n.matrix.fail = true
	n.matrix.mu.Unlock()
	post(n.h, "/in/alertmanager", "am", strings.ReplaceAll(amCritical, "Disk", "CPU"))
	failed := n.app.Audit.List(ports.AuditQuery{Verdict: ports.AuditFailed})
	if len(failed) != 1 {
		t.Fatalf("%+v", failed)
	}
	n.matrix.mu.Lock()
	n.matrix.fail = false
	n.matrix.mu.Unlock()
	rec = uiDo(ui, "POST", "/api/audit/"+failed[0].ID+"/resend?failed=true", "", true)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	eventually(t, "resent", func() bool { return len(n.matrix.list()) == 1 })
	if rec := uiDo(ui, "POST", "/api/audit/nope/resend", "", true); rec.Code != 404 {
		t.Errorf("unknown entry: %d", rec.Code)
	}
}

func TestDigestFromConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "").Replace(testConfig)
	y = strings.Replace(y, "    to: media\n", "    to: media\n    digest: 1h\n", 1)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	media := &fakeSender{}
	a.Wire(map[string]ports.Sender{"matrix/alerts": &fakeSender{}, "matrix/media": media, "hook/default": &fakeSender{}})
	go a.Deliver.Run(ctx)
	h := a.HTTP.Handler()
	post(h, "/in/sonarr", "so", `{"title":"one"}`)
	post(h, "/in/sonarr", "so", `{"title":"two"}`)
	if a.Digests.Waiting() != 2 || len(media.list()) != 0 {
		t.Fatalf("waiting %d", a.Digests.Waiting())
	}
	a.Digests.FlushAll()
	eventually(t, "digest", func() bool { return len(media.list()) == 1 })
	if m := media.list()[0].Msg.Markdown; !strings.Contains(m, "2 messages") || !strings.Contains(m, "two") {
		t.Error(m)
	}
	if _, err := config.Parse([]byte(strings.Replace(y, "    drop: true\n", "    drop: true\n    digest: 1h\n", 1))); err == nil {
		t.Error("digest on a drop rule must fail")
	}
}

func TestPingURLs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "").Replace(testConfig)
	y = strings.Replace(y, "heartbeats:\n", "heartbeats:\n  - { name: backup, token: bk, timeout: 25h, to: critical }\n", 1)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	crit := &fakeSender{}
	a.Wire(map[string]ports.Sender{"matrix/alerts": crit, "matrix/media": &fakeSender{}, "hook/default": &fakeSender{}})
	go a.Deliver.Run(ctx)
	h := a.HTTP.Handler()
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code
	}
	if get("/ping/backup?token=bk") != 200 || get("/ping/backup?token=wrong") != 404 || get("/ping/wd?token=bk") != 404 || get("/ping/nope") != 404 {
		t.Error("tokens and names")
	}
	if rec := post(h, "/ping/backup/fail", "bk", "disk full"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	eventually(t, "failure alert", func() bool { return len(crit.list()) == 1 })
	if !strings.Contains(crit.list()[0].Msg.Markdown, "disk full") {
		t.Error(crit.list()[0].Msg.Markdown)
	}
	if _, err := config.Parse([]byte(strings.Replace(y, "token: bk, ", "", 1))); err == nil {
		t.Error("a heartbeat without source and token must fail")
	}
}

func TestRuleTests(t *testing.T) {
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "").Replace(testConfig) + `
tests:
  - name: critical pages
    source: alertmanager
    body: '` + strings.ReplaceAll(amCritical, "\n", " ") + `'
    expect: { routes: [critical], to: [critical, pager], contains: ["Disk full"], loud: true, priority: high }
  - name: sonarr tests are dropped
    source: sonarr
    body: '{"eventType": "Test"}'
    expect: { dropped_by: arr-noise }
  - name: watchdog is a heartbeat
    source: alertmanager
    body: '{"version":"4","status":"firing","commonLabels":{"alertname":"Watchdog"},"alerts":[]}'
    expect: { heartbeat: wd }
  - name: wrong on purpose
    source: sonarr
    body: '{"eventType": "Download", "title": "x"}'
    expect: { to: [critical], contains: ["nope"], loud: true, priority: urgent, unmatched: true, heartbeat: x, dropped_by: y, routes: [z] }
  - { name: broken body, source: alertmanager, body: "not json" }
  - { name: no such source, source: "missing" , body: "{}" }
`
	y = strings.Replace(y, `"*": { kind: generic, token: def }`, "", 1)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a, err := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if failed := a.RunTests(&out); failed != 3 {
		t.Errorf("failed %d\n%s", failed, out.String())
	}
	o := out.String()
	for _, want := range []string{
		"ok    critical pages", "ok    sonarr tests are dropped", "ok    watchdog is a heartbeat", "FAIL  wrong on purpose",
		`to: got [media], want [critical]`, `contains "nope": the message to media is:`, "loud: the message to media is loud=false",
		`priority: the message to media has "normal", want "urgent"`, "unmatched: but it went to [media]", `heartbeat: got "", want "x"`,
		`dropped_by: got "", want "y"`, "routes: got [media], want [z]", "FAIL  broken body", "the decoder refuses the body",
		`no source "missing"`, "3 of 6 tests passed",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %q in\n%s", want, o)
		}
	}
	a.Cfg.Tests = nil
	out.Reset()
	if a.RunTests(&out) != 0 || !strings.Contains(out.String(), "no tests") {
		t.Error(out.String())
	}
}

type checkedSender struct {
	fakeSender
	problems []string
	asked    []string
}

func (c *checkedSender) Check(_ context.Context, rooms []string) []string {
	c.asked = rooms
	return c.problems
}

func TestChatCommandsAndEndpointHealth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := newNode(ctx, t, "a", "sync", "")
	if _, ok := n.app.chatCommand(ctx, "$1", "@me:x", "hello"); ok {
		t.Error("not a command")
	}
	reply, ok := n.app.chatCommand(ctx, "$1", "@me:x", "!sw mute 1h source=sonarr -- testing")
	if !ok || !strings.Contains(reply, "Muted") || len(n.app.Silences.Active()) != 1 {
		t.Fatalf("%q", reply)
	}
	if post(n.h, "/in/sonarr", "so", `{"title":"x"}`).Code != http.StatusNoContent {
		t.Error("muted from chat")
	}
	sick := &checkedSender{problems: []string{"!crit:x: the bot is leave"}}
	n.app.Endpoints = map[string]target.Target{"matrix/alerts": sick, "hook/default": &fakeSender{}}
	n.app.checkEndpoints(ctx)
	if len(sick.asked) != 2 || sick.asked[0] != "!crit:x" && sick.asked[0] != "!misc:x" {
		t.Errorf("addresses of the account's destinations: %v", sick.asked)
	}
	ok, why := n.app.health()
	if ok || !strings.Contains(why, "matrix/alerts: !crit:x: the bot is leave") {
		t.Errorf("%v %q", ok, why)
	}
	rec := httptest.NewRecorder()
	n.app.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `stellwerk_endpoint_problems{account="alerts",target="matrix",type="matrix"} 1`) {
		t.Error("problems gauge")
	}
}

func TestCanaryOverTheUI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	y := strings.NewReplacer("%NODE%", "a", "%MODE%", "sync", "%CLUSTER%", "canary: { to: misc, timeout: 2s }").Replace(testConfig)
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	alerts := &fakeSender{}
	a.Wire(map[string]ports.Sender{"matrix/alerts": alerts, "matrix/media": &fakeSender{}, "hook/default": &fakeSender{}})
	go a.Deliver.Run(ctx)
	ui := a.HTTP.UIHandler()
	if rec := uiDo(ui, "POST", "/api/canary", "", true); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(alerts.list()) != 1 || !strings.Contains(alerts.list()[0].Msg.Markdown, "Canary") {
		t.Errorf("%+v", alerts.list())
	}
	if rec := uiDo(ui, "GET", "/api/flows", "", false); !strings.Contains(rec.Body.String(), `"canaryOn":true`) || !strings.Contains(rec.Body.String(), `"canary":{`) {
		t.Error("flows show the canary")
	}
	rec := httptest.NewRecorder()
	a.Metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "stellwerk_canary_ok 1") {
		t.Error("canary metric")
	}
}
