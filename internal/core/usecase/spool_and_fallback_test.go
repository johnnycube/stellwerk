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

// ---- spool ----

func spooled(t *testing.T, senders map[string]ports.Sender) (*DeliverMessages, *portstest.Outbox, context.CancelFunc) {
	t.Helper()
	o := portstest.NewOutbox()
	d := NewDeliverMessages(senders, fast, nil, quiet()).WithOutbox(o)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return d, o, cancel
}

func TestSpool_DurableUntilDelivered(t *testing.T) {
	s := portstest.NewSender()
	s.Block()
	d, o, _ := spooled(t, map[string]ports.Sender{"m/a": s})
	done, err := d.Enqueue(domain.Delivery{Endpoint: "m/a", Message: domain.Message{Markdown: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if o.Len() != 1 {
		t.Fatal("must be in the spool before Enqueue returns")
	}
	if p, _ := o.Pending(); p[0].ID == "" {
		t.Error("spooled deliveries get an ID")
	}
	s.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eventually(t, "spool emptied", func() bool { return o.Len() == 0 })
}

func TestSpool_FailedDeliveryLeavesTheSpoolToo(t *testing.T) {
	s := portstest.NewSender()
	s.FailAll(true)
	d, o, _ := spooled(t, map[string]ports.Sender{"m/a": s})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	if err := <-done; err == nil {
		t.Fatal("expected failure")
	}
	eventually(t, "spool emptied", func() bool { return o.Len() == 0 })
}

func TestSpool_BrokenDiskRefusesWork(t *testing.T) {
	s := portstest.NewSender()
	d, o, _ := spooled(t, map[string]ports.Sender{"m/a": s})
	o.FailPut(true)
	if _, err := d.Enqueue(domain.Delivery{Endpoint: "m/a"}); !errors.Is(err, ErrSpool) {
		t.Fatalf("err %v", err)
	}
	if d.Pending() != 0 || s.Attempts() != 0 {
		t.Error("nothing may be queued that is not durable")
	}
}

func TestSpool_FullQueueRemovesFromSpool(t *testing.T) {
	s := portstest.NewSender()
	s.Block()
	defer s.Release()
	o := portstest.NewOutbox()
	d := NewDeliverMessages(map[string]ports.Sender{"m/a": s}, domain.DeliveryPolicy{QueueSize: 1, MaxRetries: 1}, nil, quiet()).WithOutbox(o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	_, _ = d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	<-s.Attempted() // first one is being sent
	if _, err := d.Enqueue(domain.Delivery{Endpoint: "m/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Enqueue(domain.Delivery{Endpoint: "m/a"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err %v", err)
	}
	if o.Len() != 2 {
		t.Errorf("refused delivery must not stay in the spool: %d", o.Len())
	}
}

func TestSpool_ShutdownKeepsUnfinishedDeliveries(t *testing.T) {
	s := portstest.NewSender()
	s.Block()
	defer s.Release()
	d, o, cancel := spooled(t, map[string]ports.Sender{"m/a": s})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	<-s.Attempted()
	cancel()
	if err := <-done; !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err %v", err)
	}
	if o.Len() != 1 {
		t.Error("an interrupted delivery must stay in the spool")
	}
}

func TestSpool_ResumesAfterRestart(t *testing.T) {
	o := portstest.NewOutbox()
	_ = o.Put(domain.Delivery{ID: "old-1", Route: "crit", Endpoint: "m/a", Address: "!x", EntryID: "gone", Message: domain.Message{Markdown: "one", Source: "am"}})
	_ = o.Put(domain.Delivery{ID: "old-2", Route: "crit", Endpoint: "removed/acc", Message: domain.Message{Markdown: "orphan", Source: "am"}})
	_ = o.Put(domain.Delivery{ID: "old-3", Route: "crit", Endpoint: "m/a", Address: "!x", Message: domain.Message{Markdown: "two", Source: "am"}})
	s := portstest.NewSender()
	store := portstest.NewAuditStore()
	clock := portstest.NewClock()
	d := NewDeliverMessages(map[string]ports.Sender{"m/a": s}, fast, nil, quiet()).WithOutbox(o).WithAudit(store, clock, "n1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	eventually(t, "resumed deliveries sent", func() bool { return len(s.Sent()) == 2 })
	if got := s.Sent(); got[0].Msg.Markdown != "one" || got[1].Msg.Markdown != "two" {
		t.Errorf("order: %+v", got)
	}
	eventually(t, "only the orphan left", func() bool { return o.Len() == 1 })
	if p, _ := o.Pending(); p[0].ID != "old-2" {
		t.Errorf("left %+v", p)
	}
	eventually(t, "audit settled", func() bool { e := store.All(); return len(e) == 1 && e[0].Settled() })
	e := store.All()[0]
	if e.Kind != "spool" || e.Source != "am" || !strings.Contains(e.Reason, "restart") || len(e.Deliveries) != 2 || e.Verdict != ports.AuditDelivered {
		t.Errorf("audit %+v", e)
	}
}

func TestSpool_ResumeWithoutAuditOrWithBrokenSpool(t *testing.T) {
	o := portstest.NewOutbox()
	_ = o.Put(domain.Delivery{ID: "x", Endpoint: "m/a"})
	s := portstest.NewSender()
	d := NewDeliverMessages(map[string]ports.Sender{"m/a": s}, fast, nil, quiet()).WithOutbox(o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	eventually(t, "sent", func() bool { return len(s.Sent()) == 1 })

	d2 := NewDeliverMessages(map[string]ports.Sender{"m/a": s}, fast, nil, quiet()).WithOutbox(brokenOutbox{})
	d2.recover(ctx) // unreadable spool: logged, nothing to resume
}

type brokenOutbox struct{}

func (brokenOutbox) Put(domain.Delivery) error           { return errors.New("x") }
func (brokenOutbox) Done(string) error                   { return errors.New("x") }
func (brokenOutbox) Pending() ([]domain.Delivery, error) { return nil, errors.New("x") }

func TestSpool_DoneFailureIsLoggedNotFatal(t *testing.T) {
	d := NewDeliverMessages(nil, fast, nil, quiet()).WithOutbox(brokenOutbox{})
	d.forget("x")
	d.forget("")
}

// ---- fallback ----

func withFallback(s *domain.Spec) {
	c := s.Destinations["critical"]
	c.Fallback = []string{"media"}
	s.Destinations["critical"] = c
}

func TestFallback_RescuesAFailedDestination(t *testing.T) {
	w := newWorld(t, withFallback)
	w.alerts.FailAll(true)
	res := w.in("{}", critical())
	if res.Outcome != ports.Delivered {
		t.Fatalf("outcome %v %s", res.Outcome, res.Detail)
	}
	got := w.media.Sent()
	if len(got) != 1 || got[0].Address != "!media:x" || !strings.Contains(got[0].Msg.Markdown, "could not deliver to `critical`") ||
		!strings.Contains(got[0].Msg.Markdown, "Disk") || got[0].Msg.Priority != domain.PriorityHigh {
		t.Fatalf("fallback message %+v", got)
	}
	eventually(t, "audit settled", func() bool { return w.last().Settled() })
	e := w.last()
	if e.Verdict != ports.AuditFallback || len(e.Deliveries) != 3 || e.Deliveries[2].Destination != "media" ||
		e.Deliveries[2].FallbackFor == nil || *e.Deliveries[2].FallbackFor != 0 || e.Deliveries[0].Status != "failed" {
		t.Errorf("audit %+v", e)
	}
}

func TestFallback_FailsWhenFallbackFailsToo(t *testing.T) {
	w := newWorld(t, withFallback)
	w.alerts.FailAll(true)
	w.media.FailAll(true)
	res := w.in("{}", critical())
	if res.Outcome != ports.Failed || !strings.Contains(res.Detail, "fallbacks failed too") {
		t.Fatalf("outcome %v %s", res.Outcome, res.Detail)
	}
	eventually(t, "audit settled", func() bool { return w.last().Settled() })
	if e := w.last(); e.Verdict != ports.AuditFailed {
		t.Errorf("verdict %s", e.Verdict)
	}
}

func TestFallback_NotUsedWhenDeliverySucceeds(t *testing.T) {
	w := newWorld(t, withFallback)
	if res := w.in("{}", critical()); res.Outcome != ports.Delivered {
		t.Fatal(res)
	}
	if len(w.media.Sent()) != 0 {
		t.Error("no fallback without a failure")
	}
}

func TestFallback_UnknownFallbackEndpointCountsAsFailure(t *testing.T) {
	d, _ := newDeliver(t, fast, map[string]ports.Sender{"m/a": failing()})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a", Fallback: []domain.Delivery{{Endpoint: "nope/x"}}})
	if err := <-done; err == nil {
		t.Error("expected failure")
	}
}

func TestFallback_OneLevelOnly(t *testing.T) {
	a, b, c := failing(), failing(), portstest.NewSender()
	d, _ := newDeliver(t, fast, map[string]ports.Sender{"a/x": a, "b/x": b, "c/x": c})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "a/x", Fallback: []domain.Delivery{
		{Endpoint: "b/x", Fallback: []domain.Delivery{{Endpoint: "c/x"}}}}})
	if err := <-done; err == nil {
		t.Error("expected failure")
	}
	time.Sleep(20 * time.Millisecond)
	if len(c.Sent()) != 0 {
		t.Error("a fallback's fallback must not be used")
	}
}

func TestFallback_HeartbeatAlarmUsesIt(t *testing.T) {
	w := newWorld(t, withFallback)
	w.coord.SetLeader(true)
	w.alerts.FailAll(true)
	w.clock.Advance(11 * time.Minute)
	w.hb.Check()
	eventually(t, "fallback sent", func() bool { return len(w.media.Sent()) == 1 })
	if m := w.media.Sent()[0].Msg; m.Priority != domain.PriorityUrgent || !strings.Contains(m.Markdown, "Heartbeat missing") {
		t.Errorf("%+v", m)
	}
}

func failing() *portstest.Sender {
	s := portstest.NewSender()
	s.FailAll(true)
	return s
}
