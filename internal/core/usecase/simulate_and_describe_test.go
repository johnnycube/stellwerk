package usecase

import (
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
)

func TestSimulateEvents_ShowsTheSameRoutingAsIngest(t *testing.T) {
	w := newWorld(t)
	sims := w.sim.Simulate([]*domain.Event{critical()})
	if len(sims) != 1 {
		t.Fatal(len(sims))
	}
	s := sims[0]
	if len(s.Deliveries) != 2 || s.Deliveries[0].Endpoint != "matrix/alerts" || s.Deliveries[1].Endpoint != "hook/default" {
		t.Errorf("deliveries %+v", s.Deliveries)
	}
	if len(s.Trace) != len(w.spec.Rules) || !s.Trace[0].Matched || s.Trace[1].Action != "skipped" {
		t.Errorf("trace %+v", s.Trace)
	}
	if !strings.Contains(s.Deliveries[0].Message.Markdown, "Disk full") {
		t.Error("rendered message missing")
	}
}

func TestSimulateEvents_HasNoSideEffects(t *testing.T) {
	w := newWorld(t)
	w.sim.Simulate([]*domain.Event{critical(), watchdog(), generic("k8s", map[string]any{"type": "Normal"})})
	time.Sleep(20 * time.Millisecond)
	if len(w.rec.Events) != 0 || w.rec.DeliveryCount() != 0 {
		t.Error("simulation recorded something")
	}
	if w.alerts.Attempts()+w.media.Attempts()+w.hook.Attempts() != 0 {
		t.Error("simulation sent something")
	}
	if _, ok := w.coord.Heartbeat("wd"); ok {
		t.Error("simulation recorded a heartbeat")
	}
	if w.coord.Seen(DedupeKey(critical(), nil, 0)) {
		t.Error("simulation claimed a dedupe key")
	}
	// and a real ingest afterwards is not a duplicate
	if res := w.in("", critical()); res.Outcome.String() != "delivered" {
		t.Errorf("ingest after simulate: %v", res.Outcome)
	}
}

func TestSimulateEvents_HeartbeatDropAndErrors(t *testing.T) {
	w := newWorld(t)
	sims := w.sim.Simulate([]*domain.Event{watchdog(), generic("k8s", map[string]any{"type": "Normal"}), generic("broken", nil), generic("nobody", nil)})
	if sims[0].Heartbeat != "wd" || len(sims[0].Trace) != 0 {
		t.Errorf("heartbeat %+v", sims[0])
	}
	if sims[1].DroppedBy != "noise" || len(sims[1].Deliveries) != 0 {
		t.Errorf("drop %+v", sims[1])
	}
	if !strings.Contains(sims[2].Err, "render") {
		t.Errorf("error %+v", sims[2])
	}
	if len(sims[3].Deliveries) != 0 || sims[3].DroppedBy != "" || sims[3].Trace[4].Reason == "" {
		t.Errorf("unmatched %+v", sims[3])
	}
}

func TestDescribeTopology_ConfigAndLiveState(t *testing.T) {
	w := newWorld(t)
	w.in("a", critical())
	w.in("hb", watchdog())
	topo := w.topo.Describe()

	if len(topo.Rules) != 5 || topo.Rules[0].Name != "critical" || topo.Rules[4].Name != "rest" {
		t.Errorf("rules must keep their order: %+v", topo.Rules)
	}
	var aliases []string
	for _, d := range topo.Destinations {
		aliases = append(aliases, d.Alias)
	}
	if !equal(aliases, []string{"critical", "media", "pager"}) || topo.Destinations[1].Account != "media" {
		t.Errorf("destinations sorted with accounts: %+v", topo.Destinations)
	}
	builtin := map[string]bool{}
	for _, tm := range topo.Templates {
		builtin[tm.Name] = tm.Builtin
	}
	if !builtin["alertmanager"] || !builtin["generic"] || builtin["custom"] || builtin["bad"] {
		t.Errorf("template origin: %v", builtin)
	}
	if topo.Cluster.NodeID != "node-a" || topo.Cluster.Leader != "node-a" {
		t.Errorf("cluster %+v", topo.Cluster)
	}
	if len(topo.Heartbeats) != 1 || topo.Heartbeats[0].LastSeen.IsZero() {
		t.Errorf("heartbeat state %+v", topo.Heartbeats)
	}
	if topo.Stats.Sources["alertmanager"].Received != 2 || topo.Stats.Routes["critical"].Matched != 1 {
		t.Errorf("stats %+v", topo.Stats)
	}
}

func TestDescribeTopology_WithoutOptionalParts(t *testing.T) {
	w := newWorld(t)
	topo := NewDescribeTopology(w.spec, w.rend, nil, w.coord, nil).Describe()
	if topo.Heartbeats != nil || topo.Stats.Sources != nil || len(topo.Templates) == 0 {
		t.Errorf("optional parts must be left empty: %+v", topo)
	}
}
