package usecase

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

func TestIngestEvents_SyncDeliversToEveryDestinationOfTheRule(t *testing.T) {
	w := newWorld(t)
	res := w.in("a", critical())
	if res.Outcome != ports.Delivered {
		t.Fatalf("outcome %v: %s", res.Outcome, res.Detail)
	}
	a, h := w.alerts.Sent(), w.hook.Sent()
	if len(a) != 1 || a[0].Address != "!crit:x" || !a[0].Msg.Loud || a[0].Msg.Mentions[0] != "@me:x" || !strings.Contains(a[0].Msg.Markdown, "Disk full") {
		t.Errorf("matrix/alerts got %+v", a)
	}
	if len(h) != 1 || h[0].Address != "/alerts" || h[0].Msg.Route != "critical" || h[0].Msg.Source != "alertmanager" {
		t.Errorf("hook got %+v", h)
	}
	if len(w.media.Sent()) != 0 {
		t.Error("media account must not be used")
	}
	ev := w.rec.Events[0]
	if ev.Verdict != "delivered" || !equal(ev.Routes, []string{"critical"}) || ev.Node != "node-a" || !ev.Time.Equal(w.clock.Now()) {
		t.Errorf("record %+v", ev)
	}
}

func TestIngestEvents_RoutesToTheRightAccount(t *testing.T) {
	w := newWorld(t)
	if res := w.in("s", generic("sonarr", map[string]any{"title": "Andor"})); res.Outcome != ports.Delivered {
		t.Fatal(res)
	}
	if m := w.media.Sent(); len(m) != 1 || m[0].Address != "!media:x" || m[0].Msg.Loud {
		t.Errorf("media got %+v", m)
	}
	if len(w.alerts.Sent())+len(w.hook.Sent()) != 0 {
		t.Error("other accounts must stay quiet")
	}
}

func TestIngestEvents_HeartbeatIsRecordedNotRouted(t *testing.T) {
	w := newWorld(t)
	w.clock.Advance(time.Minute)
	if res := w.in("hb", watchdog()); res.Outcome != ports.NothingToDo {
		t.Fatalf("outcome %v", res.Outcome)
	}
	if at, ok := w.coord.Heartbeat("wd"); !ok || !at.Equal(w.clock.Now()) {
		t.Errorf("heartbeat not stored at clock time: %v %v", at, ok)
	}
	if w.alerts.Attempts()+w.hook.Attempts() != 0 || !equal(w.rec.Verdicts(), []string{"heartbeat"}) {
		t.Errorf("heartbeat must not be delivered: %v", w.rec.Verdicts())
	}
}

func TestIngestEvents_DropAndNoMatchDeliverNothing(t *testing.T) {
	w := newWorld(t)
	if res := w.in("d", generic("k8s", map[string]any{"type": "Normal"})); res.Outcome != ports.NothingToDo {
		t.Errorf("drop: %v", res.Outcome)
	}
	if res := w.in("u", generic("unknown", map[string]any{})); res.Outcome != ports.NothingToDo {
		t.Errorf("unmatched: %v", res.Outcome)
	}
	if !equal(w.rec.Verdicts(), []string{"dropped", "unmatched"}) || w.rec.Events[0].Dropped != "noise" {
		t.Errorf("records: %+v", w.rec.Events)
	}
	if w.alerts.Attempts()+w.media.Attempts()+w.hook.Attempts() != 0 {
		t.Error("nothing may be sent")
	}
}

func TestIngestEvents_FailedDeliveryAsksForRetryAndIsNotMarkedSeen(t *testing.T) {
	w := newWorld(t)
	w.hook.FailAll(true)
	res := w.in("a", critical())
	if res.Outcome != ports.Failed || !strings.Contains(res.Detail, "fake sender down") {
		t.Fatalf("want failed, got %v %q", res.Outcome, res.Detail)
	}
	if w.hook.Attempts() != 2 {
		t.Errorf("retries: %d attempts, want MaxRetries=2", w.hook.Attempts())
	}
	w.hook.FailAll(false)
	if res := w.in("a", critical()); res.Outcome != ports.Delivered {
		t.Fatalf("the sender's retry must go through, got %v", res.Outcome)
	}
	if res := w.in("a", critical()); res.Outcome != ports.NothingToDo {
		t.Errorf("after delivery the same payload is a duplicate, got %v", res.Outcome)
	}
	if !equal(w.rec.Verdicts(), []string{"failed", "delivered", "duplicate"}) {
		t.Errorf("verdicts %v", w.rec.Verdicts())
	}
}

func TestIngestEvents_DedupeWindowExpires(t *testing.T) {
	w := newWorld(t)
	w.in("a", critical())
	w.clock.Advance(59 * time.Second)
	if res := w.in("a", critical()); res.Outcome != ports.NothingToDo {
		t.Errorf("inside window: %v", res.Outcome)
	}
	w.clock.Advance(2 * time.Second)
	if res := w.in("a", critical()); res.Outcome != ports.Delivered {
		t.Errorf("after window: %v", res.Outcome)
	}
	if res := w.in("b", critical()); res.Outcome != ports.Delivered {
		t.Errorf("a different payload is not a duplicate: %v", res.Outcome)
	}
}

func TestIngestEvents_DedupeOff(t *testing.T) {
	w := newWorld(t, func(s *domain.Spec) { s.Delivery.DedupeWindow = 0 })
	w.in("a", critical())
	if res := w.in("a", critical()); res.Outcome != ports.Delivered {
		t.Errorf("without dedupe every payload is delivered: %v", res.Outcome)
	}
	if w.coord.Seen("alertmanager:x") {
		t.Error("nothing may be claimed")
	}
}

func TestIngestEvents_AsyncAcceptsAndDeliversLater(t *testing.T) {
	w := newWorld(t, func(s *domain.Spec) { s.Delivery.Sync = false })
	if res := w.in("a", critical()); res.Outcome != ports.Accepted {
		t.Fatalf("outcome %v", res.Outcome)
	}
	eventually(t, "async delivery", func() bool { return len(w.alerts.Sent()) == 1 && len(w.hook.Sent()) == 1 })
	if res := w.in("a", critical()); res.Outcome != ports.NothingToDo {
		t.Errorf("queued payload counts as seen: %v", res.Outcome)
	}
	if !equal(w.rec.Verdicts(), []string{"queued", "duplicate"}) {
		t.Errorf("verdicts %v", w.rec.Verdicts())
	}
}

func TestIngestEvents_BusyWhenQueueIsFull(t *testing.T) {
	w := newWorld(t, func(s *domain.Spec) { s.Delivery.Sync = false; s.Delivery.QueueSize = 1; s.Delivery.DedupeWindow = 0 })
	w.media.Block()
	defer w.media.Release()
	sonarr := func() *domain.Event { return generic("sonarr", map[string]any{}) }
	w.in("1", sonarr())
	<-w.media.Attempted() // worker holds job 1
	if res := w.in("2", sonarr()); res.Outcome != ports.Accepted {
		t.Fatalf("job 2 fits in the queue: %v", res.Outcome)
	}
	res := w.in("3", sonarr())
	if res.Outcome != ports.Busy || !strings.Contains(res.Detail, "queue full") {
		t.Errorf("want busy, got %v %q", res.Outcome, res.Detail)
	}
	if got := w.rec.Verdicts(); got[len(got)-1] != "rejected" {
		t.Errorf("verdicts %v", got)
	}
	if len(w.alerts.Sent()) != 0 {
		t.Error("a full media queue must not affect other accounts")
	}
}

func TestIngestEvents_TimeoutWhenDeliveryIsSlow(t *testing.T) {
	w := newWorld(t, func(s *domain.Spec) { s.Delivery.Timeout = 30 * time.Millisecond })
	w.hook.Block()
	defer w.hook.Release()
	res := w.in("a", critical())
	if res.Outcome != ports.TimedOut || res.Detail != "delivery timed out" {
		t.Errorf("want timed out, got %v %q", res.Outcome, res.Detail)
	}
	if w.coord.Seen(DedupeKey(critical(), []byte("a"), 0)) {
		t.Error("a timed out payload must not count as seen")
	}
}

func TestIngestEvents_CancelledRequest(t *testing.T) {
	w := newWorld(t)
	w.hook.Block()
	defer w.hook.Release()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-w.hook.Attempted(); cancel() }()
	if res := w.ingest.Ingest(ctx, ports.IngestRequest{Events: []*domain.Event{critical()}, Raw: []byte("a")}); res.Outcome != ports.TimedOut || res.Detail != "request cancelled" {
		t.Errorf("got %v %q", res.Outcome, res.Detail)
	}
}

func TestIngestEvents_BrokenTemplateIsNotRetryable(t *testing.T) {
	w := newWorld(t)
	res := w.in("b", generic("broken", map[string]any{}))
	if res.Outcome != ports.Broken || !strings.Contains(res.Detail, "render") {
		t.Errorf("got %v %q", res.Outcome, res.Detail)
	}
	if !equal(w.rec.Verdicts(), []string{"error"}) {
		t.Errorf("verdicts %v", w.rec.Verdicts())
	}
}

func TestIngestEvents_SeveralEventsInOneRequest(t *testing.T) {
	w := newWorld(t)
	split := critical()
	split.Key = "fp1:firing"
	res := w.in("batch", split, watchdog(), generic("nobody", nil), generic("sonarr", map[string]any{}))
	if res.Outcome != ports.Delivered {
		t.Fatalf("outcome %v", res.Outcome)
	}
	if !equal(w.rec.Verdicts(), []string{"heartbeat", "unmatched", "delivered", "delivered"}) {
		t.Errorf("verdicts %v", w.rec.Verdicts())
	}
	if !w.coord.Seen("alertmanager:fp1:firing") {
		t.Error("an event's own key must be used for dedupe")
	}
	// the same alert in another batch is a duplicate thanks to its key
	if res := w.in("other batch", split); res.Outcome != ports.NothingToDo {
		t.Errorf("keyed duplicate: %v", res.Outcome)
	}
}

func TestIngestEvents_EmptyRequest(t *testing.T) {
	w := newWorld(t)
	if res := w.in(""); res.Outcome != ports.NothingToDo || len(w.rec.Events) != 0 {
		t.Errorf("got %v", res)
	}
}

func TestDedupeKey(t *testing.T) {
	ev := generic("s", nil)
	base := DedupeKey(ev, []byte("body"), 0)
	if base != DedupeKey(ev, []byte("body"), 0) {
		t.Error("key must be stable")
	}
	for name, other := range map[string]string{
		"body":     DedupeKey(ev, []byte("other"), 0),
		"position": DedupeKey(ev, []byte("body"), 1),
		"source":   DedupeKey(generic("t", nil), []byte("body"), 0),
	} {
		if other == base {
			t.Errorf("a different %s must give a different key", name)
		}
	}
	keyed := generic("s", nil)
	keyed.Key = "k"
	if DedupeKey(keyed, []byte("anything"), 3) != "s:k" {
		t.Error("explicit key")
	}
}
