package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

func TestCountersAndGauges(t *testing.T) {
	m := New()
	m.RecordEvent(ports.EventRecord{Source: "alertmanager", Verdict: "delivered", Routes: []string{"critical"}})
	m.RecordEvent(ports.EventRecord{Source: "alertmanager", Verdict: "delivered", Routes: []string{"critical"}})
	m.RecordEvent(ports.EventRecord{Source: "sonarr", Verdict: "dropped", Routes: []string{"noise"}})
	m.RecordDelivery(domain.Delivery{Route: "critical", Destination: "crit"}, nil)
	m.RecordDelivery(domain.Delivery{Route: "critical", Destination: "crit"}, errors.New("x"))
	m.Gauges = func() []Gauge {
		return []Gauge{
			{Name: "stellwerk_queue_pending", Help: "Queued deliveries.", Value: 3},
			{Name: "stellwerk_heartbeat_missing", Help: "1 if missing.", Labels: map[string]string{"heartbeat": "wd"}, Value: 1},
		}
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		"# TYPE stellwerk_events_total counter",
		`stellwerk_events_total{source="alertmanager",verdict="delivered"} 2`,
		`stellwerk_events_total{source="sonarr",verdict="dropped"} 1`,
		`stellwerk_route_matches_total{route="critical"} 2`,
		`stellwerk_deliveries_total{destination="crit",result="error",route="critical"} 1`,
		`stellwerk_deliveries_total{destination="crit",result="ok",route="critical"} 1`,
		"# TYPE stellwerk_queue_pending gauge\nstellwerk_queue_pending 3",
		`stellwerk_heartbeat_missing{heartbeat="wd"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type %q", ct)
	}
}

func TestLabelEscaping(t *testing.T) {
	if got := renderLabels(map[string]string{"a": "x\"y\\z\nw"}); got != `{a="x\"y\\z\nw"}` {
		t.Errorf("%s", got)
	}
	if renderLabels(nil) != "" {
		t.Error("no labels, no braces")
	}
}

func TestStableOutput(t *testing.T) {
	m := New()
	for _, s := range []string{"b", "a", "c"} {
		m.RecordEvent(ports.EventRecord{Source: s, Verdict: "delivered"})
	}
	var a, b strings.Builder
	m.Write(&a)
	m.Write(&b)
	if a.String() != b.String() || strings.Index(a.String(), `source="a"`) > strings.Index(a.String(), `source="b"`) {
		t.Error("output must be sorted and stable")
	}
}
