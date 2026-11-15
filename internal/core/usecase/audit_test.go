package usecase

import (
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func TestAudit_DeliveredEntryTellsTheWholeStory(t *testing.T) {
	w := newWorld(t)
	w.in(`{"alert":"disk"}`, critical())
	e := w.last()
	if e.Verdict != ports.AuditDelivered || e.Source != "alertmanager" || e.Status != "firing" || e.Labels["severity"] != "critical" ||
		e.Payload != `{"alert":"disk"}` || e.Node != "node-a" || !strings.HasPrefix(e.ID, "node-a-") || !e.Time.Equal(w.clock.Now()) {
		t.Errorf("entry %+v", e)
	}
	if len(e.Trace) != len(w.spec.Rules) || !e.Trace[0].Matched || e.Trace[1].Action != "skipped" {
		t.Errorf("verdict per rule: %+v", e.Trace)
	}
	if len(e.Deliveries) != 2 {
		t.Fatalf("deliveries %+v", e.Deliveries)
	}
	d := e.Deliveries[0]
	if d.Destination != "critical" || d.Endpoint != "matrix/alerts" || d.Address != "!crit:x" || !d.Loud || !strings.Contains(d.Message, "Disk full") ||
		d.Status != "sent" || len(d.Attempts) != 1 || d.Attempts[0].Error != "" || d.FinishedAt.IsZero() {
		t.Errorf("delivery %+v", d)
	}
}

func TestAudit_FailedDeliveryKeepsEveryAttemptsError(t *testing.T) {
	w := newWorld(t)
	w.hook.FailAll(true)
	w.in("x", critical())
	e := w.last()
	if e.Verdict != ports.AuditFailed {
		t.Fatalf("verdict %s", e.Verdict)
	}
	hook := e.Deliveries[1]
	if hook.Status != "failed" || len(hook.Attempts) != 2 || hook.Attempts[0].Error != "fake sender down" || hook.Attempts[1].Error != "fake sender down" {
		t.Errorf("hook delivery %+v", hook)
	}
	if e.Deliveries[0].Status != "sent" {
		t.Error("the other destination still went out")
	}
}

func TestAudit_EveryOtherVerdict(t *testing.T) {
	w := newWorld(t)
	w.in("hb", watchdog())
	if e := w.last(); e.Verdict != ports.AuditHeartbeat || e.Heartbeat != "wd" || len(e.Deliveries) != 0 {
		t.Errorf("heartbeat %+v", e)
	}
	w.in("d", generic("k8s", map[string]any{"type": "Normal"}))
	if e := w.last(); e.Verdict != ports.AuditDropped || e.DroppedBy != "noise" || !e.Trace[1].Matched {
		t.Errorf("dropped %+v", e)
	}
	w.in("u", generic("nobody", nil))
	if e := w.last(); e.Verdict != ports.AuditUnmatched || e.Trace[4].Reason == "" {
		t.Errorf("unmatched %+v", e)
	}
	w.in("a", critical())
	w.in("a", critical())
	if e := w.last(); e.Verdict != ports.AuditDuplicate || !strings.Contains(e.Reason, "dedupe") {
		t.Errorf("duplicate %+v", e)
	}
	w.in("b", generic("broken", nil))
	if e := w.last(); e.Verdict != ports.AuditError || !strings.Contains(e.Reason, "render") {
		t.Errorf("error %+v", e)
	}
}

func TestAudit_RefusedRequests(t *testing.T) {
	w := newWorld(t)
	w.ingest.Refuse(ports.RefusedRequest{Source: "sonarr", Kind: "generic", Reason: "rate limit exceeded", Raw: []byte("{}")})
	e := w.last()
	if e.Verdict != ports.AuditRejected || e.Reason != "rate limit exceeded" || e.Source != "sonarr" || e.Payload != "{}" {
		t.Errorf("%+v", e)
	}
}

func TestAudit_PayloadIsCutOrLeftOut(t *testing.T) {
	w := newWorld(t)
	w.in(strings.Repeat("ä", 40), generic("nobody", nil)) // 80 bytes, limit 64
	e := w.last()
	if !e.PayloadTruncated || len(e.Payload) > 64 || !strings.HasPrefix(e.Payload, "ää") || strings.ContainsRune(e.Payload, '�') {
		t.Errorf("cut %q %v", e.Payload, e.PayloadTruncated)
	}
	store := portstest.NewAuditStore()
	u := NewIngestEvents(w.router, nil, w.deliver, w.coord, nil, w.clock, w.spec.Delivery, quiet()).WithAudit(store, AuditPolicy{Payload: false})
	u.Ingest(w.ctx, ports.IngestRequest{Events: []*domain.Event{generic("nobody", nil)}, Raw: []byte("secret body")})
	if got := store.All()[0]; got.Payload != "" {
		t.Errorf("payload must be left out: %q", got.Payload)
	}
}

func TestAudit_QueueFullFinishesTheRest(t *testing.T) {
	w := newWorld(t, func(s *domain.Spec) { s.Delivery.Sync = false; s.Delivery.QueueSize = 1; s.Delivery.DedupeWindow = 0 })
	w.media.Block()
	defer w.media.Release()
	sonarr := func() *domain.Event { return generic("sonarr", map[string]any{}) }
	w.in("1", sonarr())
	<-w.media.Attempted()
	w.in("2", sonarr())
	w.in("3", sonarr())
	e := w.last()
	if e.Verdict != ports.AuditFailed || e.Deliveries[0].Status != "failed" || !strings.Contains(e.Deliveries[0].Attempts[0].Error, "queue full") {
		t.Errorf("%+v", e)
	}
}

func TestAudit_AsyncEntrySettlesLater(t *testing.T) {
	w := newWorld(t, func(s *domain.Spec) { s.Delivery.Sync = false })
	w.hook.Block()
	w.in("a", critical())
	id := w.last().ID
	if e, _ := w.audit.Get(id); e.Verdict != ports.AuditPending {
		t.Fatalf("while sending: %s", e.Verdict)
	}
	w.hook.Release()
	eventually(t, "settled", func() bool { e, _ := w.audit.Get(id); return e.Verdict == ports.AuditDelivered })
}

func TestAudit_HeartbeatAlertsAreAudited(t *testing.T) {
	w := newWorld(t)
	w.clock.Advance(11 * time.Minute)
	w.hb.Check()
	e := w.last()
	if e.Source != "stellwerk" || e.Kind != "heartbeat-alert" || e.Status != "missing" || e.Heartbeat != "wd" || len(e.Deliveries) != 2 {
		t.Fatalf("%+v", e)
	}
	eventually(t, "alert delivered", func() bool { got, _ := w.audit.Get(e.ID); return got.Verdict == ports.AuditDelivered })
	w.coord.SetLeader(false)
	before := len(w.audit.All())
	w.in("hb", watchdog())
	w.hb.Check() // follower: no alert, no audit entry
	if len(w.audit.All()) != before+1 {
		t.Error("only the heartbeat itself is audited on a follower")
	}
}

func TestAudit_IDsAreUnique(t *testing.T) {
	w := newWorld(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		w.in("u", generic("nobody", nil))
		id := w.last().ID
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
}

func TestAudit_Off(t *testing.T) {
	w := newWorld(t)
	u := NewIngestEvents(w.router, nil, w.deliver, w.coord, nil, w.clock, w.spec.Delivery, quiet())
	if res := u.Ingest(w.ctx, ports.IngestRequest{Events: []*domain.Event{critical()}, Raw: []byte("z")}); res.Outcome != ports.Delivered {
		t.Errorf("works without audit: %v", res.Outcome)
	}
	u.Refuse(ports.RefusedRequest{Source: "x"}) // no store, no panic
}

func TestReadAudit(t *testing.T) {
	store := portstest.NewAuditStore()
	for i := 0; i < 1200; i++ {
		store.Record(ports.AuditEntry{ID: string(rune('a'+i%26)) + time.Duration(i).String(), Source: "s", Verdict: ports.AuditUnmatched})
	}
	u := NewReadAudit(store)
	if n := len(u.List(ports.AuditQuery{})); n != defaultAuditLimit {
		t.Errorf("default limit: %d", n)
	}
	if n := len(u.List(ports.AuditQuery{Limit: 5000})); n != maxAuditLimit {
		t.Errorf("max limit: %d", n)
	}
	first := u.List(ports.AuditQuery{Limit: 1})[0]
	if got, ok := u.Get(first.ID); !ok || got.ID != first.ID {
		t.Error("get")
	}
	empty := NewReadAudit(nil)
	if empty.List(ports.AuditQuery{}) != nil {
		t.Error("no store, no entries")
	}
	if _, ok := empty.Get("x"); ok {
		t.Error("no store")
	}
}
