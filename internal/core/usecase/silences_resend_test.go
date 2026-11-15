package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func silenced(t *testing.T) (*world, *ManageSilences, *portstest.SilenceStore) {
	w := newWorld(t)
	store := portstest.NewSilenceStore()
	s := NewManageSilences(store, w.clock, quiet())
	w.ingest.WithSilences(s)
	w.hb.WithSilences(s)
	w.sim.WithSilences(s)
	w.topo.WithSilences(s)
	return w, s, store
}

func TestSilences_MuteMatchingEvents(t *testing.T) {
	w, s, store := silenced(t)
	sil, err := s.Silence(ports.SilenceRequest{Matchers: "source=alertmanager severity=critical", For: "2h", By: "@me:x", Comment: "disk swap"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sil.ID, "s-") || !sil.Ends.Equal(w.clock.Now().Add(2*time.Hour)) || sil.CreatedBy != "@me:x" {
		t.Errorf("%+v", sil)
	}
	if all, _ := store.All(); len(all) != 1 {
		t.Error("silence must be stored")
	}
	if res := w.in("{}", critical()); res.Outcome != ports.NothingToDo {
		t.Fatalf("outcome %v", res.Outcome)
	}
	if len(w.alerts.Sent()) != 0 {
		t.Error("silenced events must not be sent")
	}
	e := w.last()
	if e.Verdict != ports.AuditSilenced || e.SilencedBy != sil.ID || !strings.Contains(e.Reason, "disk swap") || len(e.Trace) == 0 {
		t.Errorf("audit %+v", e)
	}
	if sims := w.sim.Simulate([]*domain.Event{critical()}); !strings.Contains(sims[0].Silenced, sil.ID) {
		t.Errorf("simulation must say it is silenced: %+v", sims[0])
	}
	if topo := w.topo.Describe(); len(topo.Silences) != 1 {
		t.Error("topology lists silences")
	}
	if res := w.in("{}", generic("xyz", map[string]any{"text": "other"})); res.Outcome != ports.Delivered {
		t.Error("other events still go out")
	}
	w.clock.Advance(2 * time.Hour)
	if res := w.in(`{"x":1}`, critical()); res.Outcome != ports.Delivered || len(s.Active()) != 0 {
		t.Error("after the silence ends, events go out again")
	}
}

func TestSilences_HeartbeatAlarm(t *testing.T) {
	w, s, _ := silenced(t)
	w.coord.SetLeader(true)
	if _, err := s.Silence(ports.SilenceRequest{Matchers: "heartbeat=wd", For: "1h"}); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(11 * time.Minute)
	w.hb.Check()
	time.Sleep(20 * time.Millisecond)
	if len(w.alerts.Sent()) != 0 {
		t.Error("silenced heartbeat alarm must not be sent")
	}
	if e := w.last(); e.Verdict != ports.AuditSilenced || e.Heartbeat != "wd" {
		t.Errorf("%+v", e)
	}
}

func TestSilences_ExpireAndRestart(t *testing.T) {
	clock := portstest.NewClock()
	store := portstest.NewSilenceStore()
	s := NewManageSilences(store, clock, quiet())
	a, _ := s.Silence(ports.SilenceRequest{Matchers: "source=a", For: "1h"})
	b, _ := s.Silence(ports.SilenceRequest{Matchers: "source=b", For: "3h"})
	_, _ = s.Silence(ports.SilenceRequest{Matchers: "source=c", For: "30m"})
	if act := s.Active(); len(act) != 3 || act[0].Match.Source != "c" {
		t.Errorf("sorted by end: %+v", act)
	}
	if err := s.Expire(a.ID[:5]); err != nil && !strings.Contains(err.Error(), "more than one") {
		t.Fatal(err)
	}
	if err := s.Expire(a.ID); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.Expire("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	clock.Advance(time.Hour)
	again := NewManageSilences(store, clock, quiet()) // restart: expired ones are dropped
	if act := again.Active(); len(act) != 1 || act[0].ID != b.ID {
		t.Errorf("after restart %+v", act)
	}
	if all, _ := store.All(); len(all) != 1 {
		t.Errorf("expired silences are removed from the store: %d", len(all))
	}
}

func TestSilences_Errors(t *testing.T) {
	store := portstest.NewSilenceStore()
	s := NewManageSilences(store, portstest.NewClock(), quiet())
	for name, r := range map[string]ports.SilenceRequest{
		"no matcher":   {Matchers: "", For: "1h"},
		"bad duration": {Matchers: "source=a", For: "soon"},
		"too long":     {Matchers: "source=a", For: "31d"},
	} {
		if _, err := s.Silence(r); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	store.Fail = true
	if _, err := s.Silence(ports.SilenceRequest{Matchers: "source=a", For: "1h"}); err == nil {
		t.Error("store failure must be reported")
	}
	_ = store
	bad := portstest.NewSilenceStore()
	_ = bad.Save(domain.Silence{ID: "broken", Match: domain.Match{Status: "("}, Ends: time.Now().Add(time.Hour)})
	if len(NewManageSilences(bad, portstest.NewClock(), quiet()).Active()) != 0 {
		t.Error("invalid stored silence must be dropped")
	}
}

func TestResend(t *testing.T) {
	w := newWorld(t)
	w.alerts.FailAll(true)
	w.in("{}", critical())
	eventually(t, "settled", func() bool { return w.last().Settled() })
	old := w.last()
	w.alerts.FailAll(false)
	r := NewResendAudit(w.audit, w.deliver, w.router, w.clock, "node-a")

	id, err := r.Resend(old.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "resent", func() bool { return len(w.alerts.Sent()) == 1 })
	if len(w.hook.Sent()) != 1 {
		t.Error("only the failed destination is sent again")
	}
	e, _ := w.audit.Get(id)
	if e.ResentFrom != old.ID || e.Kind != "resend" || len(e.Deliveries) != 1 || e.Deliveries[0].Destination != "critical" ||
		!strings.Contains(w.alerts.Sent()[0].Msg.Markdown, "Disk") || w.alerts.Sent()[0].Msg.Priority != domain.PriorityHigh {
		t.Errorf("%+v %+v", e, w.alerts.Sent())
	}
	id2, err := r.Resend(old.ID, false)
	if err != nil || id2 == id {
		t.Fatalf("%v, ids %s %s", err, id, id2)
	}
	eventually(t, "all again", func() bool { return len(w.hook.Sent()) == 2 })

	if _, err := r.Resend("nope", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	w.in("{}", generic("k8s", map[string]any{"type": "Normal"})) // dropped, sent nothing
	if _, err := r.Resend(w.last().ID, false); err == nil {
		t.Error("nothing to send")
	}
	ok, _ := w.audit.Get(id)
	eventually(t, "settled", func() bool { e, _ := w.audit.Get(id); return e.Settled() })
	if _, err := r.Resend(ok.ID, true); err == nil || !strings.Contains(err.Error(), "nothing failed") {
		t.Errorf("nothing failed: %v", err)
	}
	gone := w.spec
	delete(gone.Destinations, "pager")
	rend, _ := domain.NewRenderer(nil)
	rules := []domain.Rule{{Name: "x", To: []string{"critical"}}}
	router, _ := domain.NewRouter(rules, gone.Destinations, rend, domain.Engines{})
	r.SetRouter(router)
	if _, err := r.Resend(old.ID, false); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Errorf("removed destination: %v", err)
	}
}

func TestReloadSwapsRouting(t *testing.T) {
	w := newWorld(t)
	rend, _ := domain.NewRenderer(nil)
	rules := []domain.Rule{{Name: "everything-to-media", To: []string{"media"}}}
	router, err := domain.NewRouter(rules, w.spec.Destinations, rend, domain.Engines{})
	if err != nil {
		t.Fatal(err)
	}
	hbs := []domain.Heartbeat{{Name: "new", Source: "cron", Timeout: time.Minute, To: []string{"media"}}}
	w.ingest.SetRouting(router, hbs)
	w.sim.SetRouting(router, hbs)
	w.hb.SetRouting(hbs, router)
	spec := w.spec
	spec.Rules, spec.Heartbeats = rules, hbs
	w.topo.SetSpec(spec, rend)
	if res := w.in("{}", critical()); res.Outcome != ports.Delivered || len(w.media.Sent()) != 1 || len(w.alerts.Sent()) != 0 {
		t.Errorf("new rules: %v", res)
	}
	if sims := w.sim.Simulate([]*domain.Event{{Source: "cron"}}); sims[0].Heartbeat != "new" {
		t.Error("simulator uses the new heartbeats")
	}
	if st := w.hb.States(); len(st) != 1 || st[0].Name != "new" {
		t.Errorf("%+v", st)
	}
	if topo := w.topo.Describe(); len(topo.Rules) != 1 {
		t.Error("topology shows the new rules")
	}
}

func TestSilences_IDsPruneAndPrefix(t *testing.T) {
	clock := portstest.NewClock()
	store := portstest.NewSilenceStore()
	s := NewManageSilences(store, clock, quiet())
	a, err := s.Silence(ports.SilenceRequest{ID: "s-chat1", Matchers: "source=a", For: "1h"})
	if err != nil || a.ID != "s-chat1" {
		t.Fatalf("%v %+v", err, a)
	}
	if again, _ := s.Silence(ports.SilenceRequest{ID: "s-chat1", Matchers: "source=other", For: "5h"}); again.Match.Source != "a" {
		t.Error("the same ID returns the existing silence")
	}
	clock.Advance(2 * time.Hour)
	_, _ = s.Silence(ports.SilenceRequest{Matchers: "source=b", For: "1h"}) // prunes the expired one
	if all, _ := store.All(); len(all) != 1 {
		t.Errorf("expired silences are pruned: %d", len(all))
	}
	if err := s.Expire("s-"); err != nil {
		t.Errorf("a unique prefix works: %v", err)
	}
	store.Fail = true
	_ = s
}

func TestHasEndpointAndDigestRun(t *testing.T) {
	d := NewDeliverMessages(map[string]ports.Sender{"m/a": portstest.NewSender()}, fast, nil, quiet())
	if !d.HasEndpoint("m/a") || d.HasEndpoint("x/y") {
		t.Error("HasEndpoint")
	}
	ctx, cancel := context.WithCancel(context.Background())
	dg := NewDigestMessages(d, portstest.NewClock(), quiet())
	done := make(chan struct{})
	go func() { dg.Run(ctx); close(done) }()
	cancel()
	<-done
	dg.finish([]domain.Delivery{{EntryID: "x"}}, nil) // no audit: nothing to do
}

func TestCanarySetRouterAndDefaults(t *testing.T) {
	w := newWorld(t)
	c := NewRunCanary(CanarySpec{To: []string{"critical"}}, w.router, w.deliver, nil, w.clock, quiet())
	if c.spec.Timeout != 2*time.Minute {
		t.Error("default timeout")
	}
	c.SetRouter(w.router)
	if res := c.Run(context.Background()); !res.OK {
		t.Error(res)
	}
}
