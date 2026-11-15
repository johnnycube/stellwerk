package portstest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// CoordinatorUnderTest is a fresh coordinator plus a way to let time pass for
// it: a manual clock for fakes, a real sleep for adapters on the system clock.
type CoordinatorUnderTest struct {
	Coordinator ports.Coordinator
	Advance     func(time.Duration)
}

// CoordinatorContract checks what every ports.Coordinator must do.
func CoordinatorContract(t *testing.T, newCoord func(t *testing.T) CoordinatorUnderTest) {
	t.Run("identity", func(t *testing.T) {
		c := newCoord(t).Coordinator
		if c.NodeID() == "" {
			t.Fatal("NodeID must not be empty")
		}
		if st := c.Status(); st.NodeID != c.NodeID() {
			t.Errorf("Status().NodeID = %q, want %q", st.NodeID, c.NodeID())
		}
		if c.IsLeader() && c.Status().Leader != c.NodeID() {
			t.Errorf("a leader must report itself as leader in Status, got %q", c.Status().Leader)
		}
	})
	t.Run("heartbeat newest wins", func(t *testing.T) {
		c := newCoord(t).Coordinator
		if _, ok := c.Heartbeat("hb"); ok {
			t.Fatal("unknown heartbeat must report false")
		}
		t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		c.SetHeartbeat("hb", t1)
		if got, ok := c.Heartbeat("hb"); !ok || !got.Equal(t1) {
			t.Fatalf("got %v %v, want %v", got, ok, t1)
		}
		c.SetHeartbeat("hb", t1.Add(time.Minute))
		c.SetHeartbeat("hb", t1.Add(-time.Hour)) // older, must be ignored
		if got, _ := c.Heartbeat("hb"); !got.Equal(t1.Add(time.Minute)) {
			t.Errorf("older heartbeat replaced newer one: %v", got)
		}
		if _, ok := c.Heartbeat("other"); ok {
			t.Error("heartbeats must be kept per name")
		}
	})
	t.Run("claim and seen", func(t *testing.T) {
		c := newCoord(t).Coordinator
		if c.Seen("k") {
			t.Fatal("unclaimed key reported as seen")
		}
		if !c.Claim("k", time.Minute) {
			t.Fatal("first claim must succeed")
		}
		if !c.Seen("k") {
			t.Error("claimed key must be seen")
		}
		if c.Claim("k", time.Minute) {
			t.Error("second claim within ttl must fail")
		}
		if c.Seen("other") {
			t.Error("keys must be independent")
		}
	})
	t.Run("claim expires", func(t *testing.T) {
		u := newCoord(t)
		ttl := 40 * time.Millisecond
		u.Coordinator.Claim("k", ttl)
		u.Advance(ttl + 20*time.Millisecond)
		if u.Coordinator.Seen("k") {
			t.Error("key must expire after ttl")
		}
		if !u.Coordinator.Claim("k", ttl) {
			t.Error("expired key must be claimable again")
		}
	})
	t.Run("run stops with context", func(t *testing.T) {
		c := newCoord(t).Coordinator
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
}

// RecorderContract checks a Recorder and the StatsReader over its data.
func RecorderContract(t *testing.T, newRec func() (ports.Recorder, ports.StatsReader)) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t.Run("events are counted per source and route, newest first", func(t *testing.T) {
		rec, rd := newRec()
		rec.RecordEvent(ports.EventRecord{Time: at, Source: "am", Routes: []string{"crit"}, Verdict: "delivered"})
		rec.RecordEvent(ports.EventRecord{Time: at.Add(time.Second), Source: "am", Routes: []string{"crit"}, Verdict: "delivered"})
		rec.RecordEvent(ports.EventRecord{Time: at.Add(2 * time.Second), Source: "sonarr", Dropped: "noise", Routes: []string{"noise"}, Verdict: "dropped"})
		s := rd.Snapshot()
		if s.Sources["am"].Received != 2 || s.Sources["sonarr"].Received != 1 || !s.Sources["am"].LastSeen.Equal(at.Add(time.Second)) {
			t.Errorf("sources: %+v", s.Sources)
		}
		if s.Routes["crit"].Matched != 2 || s.Routes["noise"].Matched != 1 {
			t.Errorf("routes: %+v", s.Routes)
		}
		if len(s.Events) != 3 || s.Events[0].Source != "sonarr" || s.Events[2].Source != "am" {
			t.Errorf("events must be newest first: %+v", s.Events)
		}
	})
	t.Run("deliveries count per route and destination", func(t *testing.T) {
		rec, rd := newRec()
		d := domain.Delivery{Route: "crit", Destination: "critical", Endpoint: "m/a"}
		rec.RecordDelivery(d, nil)
		rec.RecordDelivery(d, errors.New("room gone"))
		s := rd.Snapshot()
		if r := s.Routes["crit"]; r.Delivered != 1 || r.Failed != 1 || r.LastError != "room gone" {
			t.Errorf("route: %+v", r)
		}
		if ds := s.Destinations["critical"]; ds.Delivered != 1 || ds.Failed != 1 {
			t.Errorf("destination: %+v", ds)
		}
	})
	t.Run("empty snapshot has usable maps", func(t *testing.T) {
		_, rd := newRec()
		s := rd.Snapshot()
		if s.Sources == nil || s.Routes == nil || s.Destinations == nil {
			t.Error("maps must not be nil")
		}
	})
}

// SenderUnderTest is a sender wired to something that can tell what arrived.
type SenderUnderTest struct {
	Sender ports.Sender
	// Received returns what arrived, as address and message.
	Received func() []Sent
	// Break makes the next sends fail (the backend is down).
	Break func()
	// AddressHint is a valid address for this sender, default "/room-1".
	AddressHint string
	// BodyOnly: the backend keeps only address and text (mail, push), so
	// only those are compared.
	BodyOnly bool
}

func (u SenderUnderTest) address() string {
	if u.AddressHint != "" {
		return u.AddressHint
	}
	return "/room-1"
}

// SenderContract checks what every ports.Sender must do.
func SenderContract(t *testing.T, newSender func(t *testing.T) SenderUnderTest) {
	msg := domain.Message{Markdown: "**disk full**", Loud: true, Mentions: []string{"@me:x"}, Source: "am", Route: "crit"}
	t.Run("delivers the message to the address", func(t *testing.T) {
		u := newSender(t)
		addr := u.address()
		if err := u.Sender.Send(context.Background(), addr, msg); err != nil {
			t.Fatal(err)
		}
		got := u.Received()
		if len(got) != 1 || got[0].Address != addr || got[0].Msg.Markdown != msg.Markdown ||
			(!u.BodyOnly && (got[0].Msg.Loud != msg.Loud || got[0].Msg.Route != "crit")) {
			t.Errorf("received %+v", got)
		}
	})
	t.Run("reports failure", func(t *testing.T) {
		u := newSender(t)
		u.Break()
		if err := u.Sender.Send(context.Background(), u.address(), msg); err == nil {
			t.Error("a failed send must return an error")
		}
	})
	t.Run("honours a cancelled context", func(t *testing.T) {
		u := newSender(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := u.Sender.Send(ctx, u.address(), msg); err == nil {
			t.Error("send with a cancelled context must fail")
		}
	})
}

// ClockContract checks a ports.Clock.
func ClockContract(t *testing.T, c ports.Clock) {
	a := c.Now()
	b := c.Now()
	if a.IsZero() || b.Before(a) {
		t.Errorf("clock must be set and not go backwards: %v then %v", a, b)
	}
}

// AuditStoreContract checks what every ports.AuditStore must do.
func AuditStoreContract(t *testing.T, newStore func() ports.AuditStore) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pending := func(id string) ports.AuditEntry {
		return ports.AuditEntry{
			ID: id, Time: at, Source: "am", Verdict: ports.AuditPending, Labels: map[string]string{"severity": "critical"},
			Payload: `{"alert":"Disk full"}`,
			Deliveries: []ports.AuditDelivery{
				{Route: "crit", Destination: "critical", Endpoint: "matrix/alerts", Status: "pending", Message: "disk"},
				{Route: "crit", Destination: "pager", Endpoint: "hook/default", Status: "pending"},
			},
		}
	}
	t.Run("record and get return copies", func(t *testing.T) {
		s := newStore()
		s.Record(pending("a"))
		got, ok := s.Get("a")
		if !ok || got.Source != "am" || len(got.Deliveries) != 2 || got.Labels["severity"] != "critical" {
			t.Fatalf("got %+v %v", got, ok)
		}
		got.Labels["severity"] = "changed"
		got.Deliveries[0].Status = "changed"
		again, _ := s.Get("a")
		if again.Labels["severity"] != "critical" || again.Deliveries[0].Status != "pending" {
			t.Error("callers must not be able to change stored entries")
		}
		if _, ok := s.Get("missing"); ok {
			t.Error("unknown id")
		}
	})
	t.Run("attempts and finish settle the entry", func(t *testing.T) {
		s := newStore()
		s.Record(pending("a"))
		s.Attempt("a", 0, ports.AuditAttempt{Time: at, Error: "room not found"})
		s.Attempt("a", 0, ports.AuditAttempt{Time: at.Add(time.Second)})
		s.Finish("a", 0, at.Add(time.Second), nil)
		if e, _ := s.Get("a"); e.Verdict != ports.AuditPending || e.Deliveries[0].Status != "sent" || len(e.Deliveries[0].Attempts) != 2 {
			t.Fatalf("one of two finished: %+v", e)
		}
		s.Attempt("a", 1, ports.AuditAttempt{Time: at, Error: "503"})
		s.Finish("a", 1, at.Add(2*time.Second), errors.New("503"))
		e, _ := s.Get("a")
		if e.Verdict != ports.AuditFailed || e.Deliveries[1].Status != "failed" || e.Deliveries[1].FinishedAt.IsZero() || len(e.Deliveries[1].Attempts) != 1 {
			t.Errorf("settled: %+v", e)
		}
		s.Finish("a", 1, at, nil) // finishing twice changes nothing
		if e2, _ := s.Get("a"); e2.Deliveries[1].Status != "failed" {
			t.Error("a finished delivery must stay finished")
		}
	})
	t.Run("all sent means delivered", func(t *testing.T) {
		s := newStore()
		s.Record(pending("a"))
		s.Finish("a", 0, at, nil)
		s.Finish("a", 1, at, nil)
		if e, _ := s.Get("a"); e.Verdict != ports.AuditDelivered {
			t.Errorf("verdict %s", e.Verdict)
		}
	})
	t.Run("failure before any attempt is recorded", func(t *testing.T) {
		s := newStore()
		s.Record(pending("a"))
		s.Finish("a", 0, at, errors.New("queue full"))
		if e, _ := s.Get("a"); len(e.Deliveries[0].Attempts) != 1 || e.Deliveries[0].Attempts[0].Error != "queue full" {
			t.Errorf("%+v", e.Deliveries[0])
		}
	})
	t.Run("a fallback that delivers rescues the failed delivery", func(t *testing.T) {
		s := newStore()
		s.Record(pending("a"))
		s.Finish("a", 1, at, nil)
		zero := 0
		fb := s.AddDelivery("a", ports.AuditDelivery{Route: "crit", Destination: "mail", Endpoint: "mail/default", FallbackFor: &zero})
		if fb != 2 {
			t.Fatalf("fallback index %d", fb)
		}
		s.Finish("a", 0, at, errors.New("homeserver down"))
		if e, _ := s.Get("a"); e.Verdict != ports.AuditPending || e.Deliveries[2].Status != "pending" {
			t.Fatalf("must wait for the fallback: %+v", e)
		}
		s.Finish("a", fb, at, nil)
		e, _ := s.Get("a")
		if e.Verdict != ports.AuditFallback || *e.Deliveries[2].FallbackFor != 0 {
			t.Errorf("verdict %s", e.Verdict)
		}
		if s.AddDelivery("a", ports.AuditDelivery{}) != -1 || s.AddDelivery("nope", ports.AuditDelivery{}) != -1 {
			t.Error("settled or unknown entries take no deliveries")
		}
	})
	t.Run("a fallback that fails too means failed", func(t *testing.T) {
		s := newStore()
		s.Record(pending("a"))
		s.Finish("a", 1, at, nil)
		zero := 0
		fb := s.AddDelivery("a", ports.AuditDelivery{Destination: "mail", FallbackFor: &zero})
		s.Finish("a", 0, at, errors.New("down"))
		s.Finish("a", fb, at, errors.New("smtp down"))
		if e, _ := s.Get("a"); e.Verdict != ports.AuditFailed {
			t.Errorf("verdict %s", e.Verdict)
		}
	})
	t.Run("unknown ids and indexes are ignored", func(_ *testing.T) {
		s := newStore()
		s.Attempt("nope", 0, ports.AuditAttempt{})
		s.Finish("nope", 0, at, nil)
		s.Record(pending("a"))
		s.Attempt("a", 9, ports.AuditAttempt{})
		s.Finish("a", -1, at, nil)
	})
	t.Run("list newest first with filters", func(t *testing.T) {
		s := newStore()
		s.Record(ports.AuditEntry{ID: "1", Time: at, Source: "am", Verdict: ports.AuditDropped, DroppedBy: "noise"})
		s.Record(ports.AuditEntry{ID: "2", Time: at.Add(time.Second), Source: "sonarr", Verdict: ports.AuditRejected, Reason: "rate limit exceeded"})
		s.Record(pending("3"))
		ids := func(es []ports.AuditEntry) string {
			var out []string
			for _, e := range es {
				out = append(out, e.ID)
			}
			return strings.Join(out, ",")
		}
		cases := map[string]ports.AuditQuery{
			"3,2,1": {},
			"3,1":   {Source: "am"},
			"2":     {Verdict: ports.AuditRejected},
			"3":     {Text: "DISK FULL"},
			"1":     {Text: "noise"},
			"2,":    {Text: "rate LIMIT"},
			"3,2":   {Limit: 2},
			"":      {Source: "am", Verdict: ports.AuditRejected},
		}
		for want, q := range cases {
			want = strings.TrimSuffix(want, ",")
			if got := ids(s.List(q)); got != want {
				t.Errorf("%+v: got %q want %q", q, got, want)
			}
		}
		s.Attempt("3", 1, ports.AuditAttempt{Error: "connection refused"})
		if got := ids(s.List(ports.AuditQuery{Text: "refused"})); got != "3" {
			t.Errorf("search in attempt errors: %q", got)
		}
	})
}

// OutboxContract checks what every ports.Outbox must do. newOutbox may be
// called several times with the same t: each call must see what earlier
// ones stored (reopen), so durable stores prove they survive a restart.
func OutboxContract(t *testing.T, newOutbox func(t *testing.T) ports.Outbox) {
	d := func(id string) domain.Delivery {
		return domain.Delivery{
			ID: id, Route: "crit", Destination: "critical", Endpoint: "matrix/alerts", Address: "!r:x",
			EntryID: "node-1", Index: 1, Created: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC),
			Message: domain.Message{Markdown: "**disk** full", Loud: true, Priority: domain.PriorityUrgent, Mentions: []string{"@me:x"}, Source: "am", Route: "crit"},
			Fallback: []domain.Delivery{{
				Route: "crit", Destination: "mail", Endpoint: "mail/default", Address: "me@x",
				Message: domain.Message{Markdown: "fb"}, FallbackFor: "critical",
			}},
		}
	}
	t.Run("put, pending in order, done", func(t *testing.T) {
		o := newOutbox(t)
		for _, id := range []string{"c", "a", "b"} {
			if err := o.Put(d(id)); err != nil {
				t.Fatal(err)
			}
		}
		got, err := o.Pending()
		if err != nil || len(got) != 3 || got[0].ID != "c" || got[1].ID != "a" || got[2].ID != "b" {
			t.Fatalf("%v %+v", err, got)
		}
		if err := o.Done("a"); err != nil {
			t.Fatal(err)
		}
		if err := o.Done("never-put"); err != nil {
			t.Errorf("unknown id: %v", err)
		}
		got, _ = o.Pending()
		if len(got) != 2 || got[0].ID != "c" || got[1].ID != "b" {
			t.Errorf("%+v", got)
		}
	})
	t.Run("every field survives", func(t *testing.T) {
		o := newOutbox(t)
		want := d("x")
		if err := o.Put(want); err != nil {
			t.Fatal(err)
		}
		got, _ := o.Pending()
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Errorf("got  %+v\nwant %+v", got, want)
		}
	})
	t.Run("put again replaces and keeps the position", func(t *testing.T) {
		o := newOutbox(t)
		_ = o.Put(d("a"))
		_ = o.Put(d("b"))
		changed := d("a")
		changed.Address = "!other:x"
		_ = o.Put(changed)
		got, _ := o.Pending()
		if len(got) != 2 || got[0].ID != "a" || got[0].Address != "!other:x" {
			t.Errorf("%+v", got)
		}
	})
	t.Run("survives reopening", func(t *testing.T) {
		o := newOutbox(t)
		_ = o.Put(d("a"))
		_ = o.Put(d("b"))
		_ = o.Done("a")
		again := newOutbox(t)
		got, _ := again.Pending()
		if len(got) != 1 || got[0].ID != "b" {
			t.Errorf("after reopen: %+v", got)
		}
	})
}

// SilenceStoreContract checks what every ports.SilenceStore must do. As in
// OutboxContract, calling newStore again with the same t reopens.
func SilenceStoreContract(t *testing.T, newStore func(t *testing.T) ports.SilenceStore) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	x := domain.Silence{
		ID: "s1", Match: domain.Match{Source: "k8s*", Status: "firing", Labels: map[string]string{"severity": "warning"}},
		Starts: at, Ends: at.Add(time.Hour), CreatedBy: "@me:x", Comment: "upgrade",
	}
	t.Run("save, all, delete", func(t *testing.T) {
		s := newStore(t)
		if err := s.Save(x); err != nil {
			t.Fatal(err)
		}
		y := x
		y.ID = "s2"
		_ = s.Save(y)
		all, err := s.All()
		if err != nil || len(all) != 2 {
			t.Fatalf("%v %+v", err, all)
		}
		if err := s.Delete("s1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete("unknown"); err != nil {
			t.Errorf("unknown id: %v", err)
		}
		all, _ = s.All()
		if len(all) != 1 || all[0].ID != "s2" {
			t.Errorf("%+v", all)
		}
	})
	t.Run("every field survives and reopening keeps it", func(t *testing.T) {
		s := newStore(t)
		_ = s.Save(x)
		changed := x
		changed.Comment = "longer upgrade"
		_ = s.Save(changed) // same ID replaces
		all, _ := newStore(t).All()
		if len(all) != 1 || !reflect.DeepEqual(all[0], changed) {
			t.Errorf("got %+v", all)
		}
	})
}
