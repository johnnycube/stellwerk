package usecase

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

// digestWorld routes media events into an hourly digest.
func digestWorld(t *testing.T) (*world, *DigestMessages) {
	w := newWorld(t, func(s *domain.Spec) {
		for i := range s.Rules {
			if s.Rules[i].Name == "media" {
				s.Rules[i].Digest = time.Hour
			}
		}
		m := s.Destinations["media"]
		m.Fallback = []string{"pager"}
		s.Destinations["media"] = m
	})
	dg := NewDigestMessages(w.deliver, w.clock, quiet()).WithAudit(w.audit)
	w.ingest = NewIngestEvents(w.router, w.spec.Heartbeats, dg, w.coord, w.rec, w.clock, w.spec.Delivery, quiet()).WithAudit(w.audit, AuditPolicy{})
	return w, dg
}

func TestDigest_CollectsAndSendsOneSummary(t *testing.T) {
	w, dg := digestWorld(t)
	for _, title := range []string{"Severance S02E01", "Severance S02E02", "Andor S02E01"} {
		if res := w.in(title, generic("sonarr", map[string]any{"title": title})); res.Outcome != ports.Delivered {
			t.Fatalf("%v", res)
		}
	}
	if res := w.in("{}", critical()); res.Outcome != ports.Delivered || len(w.alerts.Sent()) != 1 {
		t.Fatal("routes without digest are not held back")
	}
	if dg.Waiting() != 3 || len(w.media.Sent()) != 0 {
		t.Fatalf("waiting %d, sent %d", dg.Waiting(), len(w.media.Sent()))
	}
	if e := w.audit.All()[0]; e.Verdict != ports.AuditPending {
		t.Error("collected messages stay pending in the audit log")
	}
	w.clock.Advance(59 * time.Minute)
	dg.Flush(false)
	if len(w.media.Sent()) != 0 {
		t.Fatal("not before the window ends")
	}
	w.clock.Advance(time.Minute)
	dg.Flush(false)
	eventually(t, "digest sent", func() bool { return len(w.media.Sent()) == 1 })
	m := w.media.Sent()[0].Msg.Markdown
	if !strings.HasPrefix(m, "📬 **Digest**: 3 messages from `media` in the last 1h0m0s") || strings.Count(m, "---") != 3 ||
		!strings.Contains(m, "Andor S02E01") || dg.Waiting() != 0 {
		t.Errorf("%q", m)
	}
	eventually(t, "audit settled", func() bool { return w.audit.All()[0].Settled() })
	if e := w.audit.All()[0]; e.Verdict != ports.AuditDelivered {
		t.Errorf("%+v", e)
	}
}

func TestDigest_FlushAllOnShutdown(t *testing.T) {
	w, dg := digestWorld(t)
	w.in("x", generic("radarr", map[string]any{"title": "Dune"}))
	dg.FlushAll()
	eventually(t, "sent early", func() bool { return len(w.media.Sent()) == 1 })
	if !strings.Contains(w.media.Sent()[0].Msg.Markdown, "1 message from") {
		t.Error(w.media.Sent()[0].Msg.Markdown)
	}
}

func TestDigest_FailureReachesAuditAndFallback(t *testing.T) {
	w, dg := digestWorld(t)
	w.media.FailAll(true)
	w.in("x", generic("sonarr", map[string]any{"title": "a"}))
	dg.FlushAll()
	eventually(t, "fallback", func() bool { return len(w.hook.Sent()) == 1 })
	if m := w.hook.Sent()[0].Msg.Markdown; !strings.Contains(m, "Digest") || !strings.HasSuffix(m, domain.FallbackNote("media")) {
		t.Errorf("%q", m)
	}
	eventually(t, "audit settled", func() bool { return w.audit.All()[0].Settled() })
	if e := w.audit.All()[0]; e.Verdict != ports.AuditDelivered {
		t.Errorf("fallback delivered the digest, the item counts as delivered: %+v", e.Verdict)
	}
}

func TestDigest_QueueErrorFinishesItems(t *testing.T) {
	store := portstest.NewAuditStore()
	clock := portstest.NewClock()
	dg := NewDigestMessages(refusing{}, clock, quiet()).WithAudit(store)
	store.Record(ports.AuditEntry{ID: "e1", Verdict: ports.AuditPending, Deliveries: []ports.AuditDelivery{{Status: "pending"}}})
	_, _ = dg.Enqueue(domain.Delivery{Route: "r", Destination: "d", Digest: time.Minute, EntryID: "e1"})
	dg.FlushAll()
	if e, _ := store.Get("e1"); e.Verdict != ports.AuditFailed || !strings.Contains(e.Deliveries[0].Attempts[0].Error, "digest: full") {
		t.Errorf("%+v", e)
	}
}

type refusing struct{}

func (refusing) Enqueue(domain.Delivery) (<-chan error, error) { return nil, errors.New("full") }

func TestDigestBuilder(t *testing.T) {
	big := strings.Repeat("x", 4000)
	items := []domain.Delivery{
		{Route: "r", Destination: "d", Message: domain.Message{Markdown: "1" + big, Priority: domain.PriorityLow}},
		{Route: "r", Destination: "d", Message: domain.Message{Markdown: "2" + big, Priority: domain.PriorityHigh, Loud: true}},
		{Route: "r", Destination: "d", Message: domain.Message{Markdown: "3" + big}},
		{Route: "r", Destination: "d", Message: domain.Message{Markdown: "4" + big}},
	}
	d := Digest(items, 30*time.Minute)
	if len(d.Message.Markdown) > digestMaxChars+100 || !strings.Contains(d.Message.Markdown, "…and 2 more") {
		t.Errorf("len %d", len(d.Message.Markdown))
	}
	if d.Message.Priority != domain.PriorityHigh || !d.Message.Loud || d.Digest != 0 {
		t.Errorf("%+v", d.Message)
	}
}

func TestDigestBuilder_CollapsesIdenticalMessages(t *testing.T) {
	msg := func(md string) domain.Delivery {
		return domain.Delivery{Route: "r", Destination: "d", Message: domain.Message{Markdown: md}}
	}
	items := []domain.Delivery{
		msg("🧹 **Stalled**\nSome.Release"),
		msg("lidarr · DownloadFailure"),
		msg("🧹 **Stalled**\nSome.Release"),
		msg("radarr · Download"),
		msg("lidarr · DownloadFailure"),
		msg("🧹 **Stalled**\nSome.Release"),
	}
	m := Digest(items, time.Hour).Message.Markdown
	want := "📬 **Digest**: 6 messages from `r` in the last 1h0m0s" +
		"\n\n---\n\n🧹 **Stalled** ×3\nSome.Release" +
		"\n\n---\n\nlidarr · DownloadFailure ×2" +
		"\n\n---\n\nradarr · Download"
	if m != want {
		t.Errorf("got\n%s\nwant\n%s", m, want)
	}
}
