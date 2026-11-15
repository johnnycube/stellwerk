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

func deliverWith(t *testing.T, p domain.DeliveryPolicy, clock ports.Clock, senders map[string]ports.Sender) *DeliverMessages {
	t.Helper()
	d := NewDeliverMessages(senders, p, nil, quiet())
	if clock != nil {
		d.clock = clock
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return d
}

func TestRetry_PermanentErrorsAreNotRetried(t *testing.T) {
	s := portstest.NewSender()
	s.Err = ports.Permanent(errors.New("room gone"))
	s.FailAll(true)
	d := deliverWith(t, domain.DeliveryPolicy{MaxRetries: 5, MaxBackoff: time.Millisecond}, nil, map[string]ports.Sender{"m/a": s})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	if err := <-done; !ports.IsPermanent(err) || s.Attempts() != 1 {
		t.Errorf("err %v attempts %d", err, s.Attempts())
	}
}

func TestRetry_HonoursRetryAfter(t *testing.T) {
	s := portstest.NewSender()
	s.Err = ports.RetryAfter(errors.New("slow down"), 150*time.Millisecond)
	s.FailNext(1)
	d := deliverWith(t, domain.DeliveryPolicy{MaxRetries: 3, MaxBackoff: time.Millisecond}, nil, map[string]ports.Sender{"m/a": s})
	start := time.Now()
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < 140*time.Millisecond {
		t.Errorf("waited only %v, the backend asked for 150ms", waited)
	}
}

func TestRetry_Jitter(t *testing.T) {
	for range 100 {
		if j := jitter(time.Second); j < 800*time.Millisecond || j > 1200*time.Millisecond {
			t.Fatalf("%v out of ±20%%", j)
		}
	}
}

func TestBreaker_OpensSkipsAndCloses(t *testing.T) {
	clock := portstest.NewClock()
	down := portstest.NewSender()
	down.FailAll(true)
	backup := portstest.NewSender()
	d := deliverWith(t, domain.DeliveryPolicy{MaxRetries: 2, MaxBackoff: time.Millisecond, BreakerFailures: 2, BreakerCooldown: time.Minute},
		clock, map[string]ports.Sender{"m/a": down, "b/x": backup})
	send := func() error {
		done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a", Fallback: []domain.Delivery{{Endpoint: "b/x"}}})
		return <-done
	}
	_ = send()
	_ = send()
	if down.Attempts() != 4 || len(backup.Sent()) != 2 {
		t.Fatalf("attempts %d, fallbacks %d", down.Attempts(), len(backup.Sent()))
	}
	if st := d.Breakers()["m/a"]; st.Failures != 2 || !st.OpenUntil.After(clock.Now()) {
		t.Fatalf("breaker %+v", st)
	}
	if err := send(); err != nil {
		t.Fatalf("fallback must still deliver: %v", err)
	}
	if down.Attempts() != 4 || len(backup.Sent()) != 3 {
		t.Errorf("open breaker must skip the endpoint: attempts %d", down.Attempts())
	}
	clock.Advance(2 * time.Minute)
	down.FailAll(false)
	_ = send()
	if down.Attempts() != 5 || d.Breakers()["m/a"].Failures != 0 {
		t.Errorf("after cooldown one try, success closes: %d %+v", down.Attempts(), d.Breakers()["m/a"])
	}
}

func TestBreaker_PermanentErrorsDoNotCount(t *testing.T) {
	s := portstest.NewSender()
	s.Err = ports.Permanent(errors.New("bad address"))
	s.FailAll(true)
	d := deliverWith(t, domain.DeliveryPolicy{MaxRetries: 1, BreakerFailures: 1, BreakerCooldown: time.Minute}, nil, map[string]ports.Sender{"m/a": s})
	for range 3 {
		done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
		<-done
	}
	if s.Attempts() != 3 {
		t.Error("one bad address must not take the whole account offline")
	}
}

func TestAge_LateMessagesGetANote(t *testing.T) {
	clock := portstest.NewClock()
	s := portstest.NewSender()
	d := deliverWith(t, domain.DeliveryPolicy{MaxRetries: 1, LateAfter: 10 * time.Minute}, clock, map[string]ports.Sender{"m/a": s})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a", Created: clock.Now().Add(-3*time.Hour - 12*time.Minute), Message: domain.Message{Markdown: "disk full"}})
	<-done
	done, _ = d.Enqueue(domain.Delivery{Endpoint: "m/a", Message: domain.Message{Markdown: "fresh"}})
	<-done
	got := s.Sent()
	if !strings.HasPrefix(got[0].Msg.Markdown, "disk full\n\n⏱ _Late: this was received 3h12m0s ago._") || got[1].Msg.Markdown != "fresh" {
		t.Errorf("%q / %q", got[0].Msg.Markdown, got[1].Msg.Markdown)
	}
}

func TestAge_TooOldIsNotSentAndGetsNoFallback(t *testing.T) {
	clock := portstest.NewClock()
	s, fb := portstest.NewSender(), portstest.NewSender()
	d := deliverWith(t, domain.DeliveryPolicy{MaxRetries: 1, MaxAge: time.Hour}, clock, map[string]ports.Sender{"m/a": s, "b/x": fb})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a", Created: clock.Now().Add(-2 * time.Hour), Fallback: []domain.Delivery{{Endpoint: "b/x"}}})
	if err := <-done; !errors.Is(err, ErrExpired) || s.Attempts() != 0 || fb.Attempts() != 0 {
		t.Errorf("err %v attempts %d/%d", err, s.Attempts(), fb.Attempts())
	}
}

func TestAge_FallbackKeepsTheOriginalTime(t *testing.T) {
	clock := portstest.NewClock()
	down, fb := failing(), portstest.NewSender()
	d := deliverWith(t, domain.DeliveryPolicy{MaxRetries: 1, LateAfter: time.Minute}, clock, map[string]ports.Sender{"m/a": down, "b/x": fb})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a", Created: clock.Now().Add(-5 * time.Minute), Fallback: []domain.Delivery{{Endpoint: "b/x"}}})
	<-done
	if m := fb.Sent()[0].Msg.Markdown; !strings.Contains(m, "Late: this was received 5m0s ago") {
		t.Errorf("%q", m)
	}
}

func TestRoundAge(t *testing.T) {
	for d, want := range map[time.Duration]string{3*time.Hour + 12*time.Minute + 40*time.Second: "3h13m0s", 90 * time.Second: "1m30s", 1500 * time.Millisecond: "1.5s"} {
		if got := roundAge(d); got != want {
			t.Errorf("%v: %s", d, got)
		}
	}
}
