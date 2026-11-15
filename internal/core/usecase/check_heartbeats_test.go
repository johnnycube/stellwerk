package usecase

import (
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

// waitSent waits until n messages arrived at s.
func waitSent(t *testing.T, s *portstest.Sender, n int) []portstest.Sent {
	t.Helper()
	eventually(t, "messages", func() bool { return len(s.Sent()) >= n })
	time.Sleep(10 * time.Millisecond) // and make sure no more come
	return s.Sent()
}

func TestCheckHeartbeats_GracePeriodAfterStart(t *testing.T) {
	w := newWorld(t)
	w.clock.Advance(9 * time.Minute)
	if ch := w.hb.Check(); len(ch) != 0 {
		t.Errorf("a heartbeat never seen gets the timeout as grace period: %+v", ch)
	}
	w.clock.Advance(2 * time.Minute)
	ch := w.hb.Check()
	if len(ch) != 1 || ch[0].Name != "wd" || !ch[0].Missing || !ch[0].Sent {
		t.Fatalf("changes %+v", ch)
	}
}

func TestCheckHeartbeats_AlertsOnceToEveryDestination(t *testing.T) {
	w := newWorld(t)
	w.in("hb", watchdog())
	w.clock.Advance(11 * time.Minute)
	w.hb.Check()
	if ch := w.hb.Check(); len(ch) != 0 {
		t.Errorf("second check must not alert again: %+v", ch)
	}
	a := waitSent(t, w.alerts, 1)
	h := waitSent(t, w.hook, 1)
	if len(a) != 1 || len(h) != 1 {
		t.Fatalf("alerts=%d hook=%d", len(a), len(h))
	}
	m := a[0].Msg
	if !m.Loud || m.Mentions[0] != "@me:x" || !strings.Contains(m.Markdown, "Heartbeat missing") || !strings.Contains(m.Markdown, "11m0s") || m.Route != "heartbeat:wd" {
		t.Errorf("alert %+v", m)
	}
}

func TestCheckHeartbeats_Recovery(t *testing.T) {
	w := newWorld(t)
	w.clock.Advance(11 * time.Minute)
	w.hb.Check()
	w.in("hb", watchdog())
	ch := w.hb.Check()
	if len(ch) != 1 || ch[0].Missing || !ch[0].Sent {
		t.Fatalf("changes %+v", ch)
	}
	a := waitSent(t, w.alerts, 2)
	if m := a[1].Msg; m.Loud || len(m.Mentions) != 0 || !strings.Contains(m.Markdown, "restored") {
		t.Errorf("recovery must be quiet and without mention: %+v", m)
	}
}

func TestCheckHeartbeats_OnlyTheLeaderSends(t *testing.T) {
	w := newWorld(t)
	w.coord.SetLeader(false)
	w.clock.Advance(11 * time.Minute)
	ch := w.hb.Check()
	if len(ch) != 1 || !ch[0].Missing || ch[0].Sent {
		t.Fatalf("a follower tracks but does not send: %+v", ch)
	}
	time.Sleep(20 * time.Millisecond)
	if w.alerts.Attempts()+w.hook.Attempts() != 0 {
		t.Error("follower sent an alert")
	}
	if st := w.hb.States(); !st[0].Missing {
		t.Error("the follower must know the heartbeat is missing")
	}
}

func TestCheckHeartbeats_NewLeaderDoesNotRepeatTheAlert(t *testing.T) {
	w := newWorld(t)
	w.coord.SetLeader(false)
	w.clock.Advance(11 * time.Minute)
	w.hb.Check() // the old leader sent the alert, this node only noted it
	w.coord.SetLeader(true)
	if ch := w.hb.Check(); len(ch) != 0 {
		t.Errorf("taking over must not repeat the alert: %+v", ch)
	}
	w.in("hb", watchdog())
	ch := w.hb.Check()
	if len(ch) != 1 || ch[0].Missing || !ch[0].Sent {
		t.Fatalf("the new leader sends the recovery: %+v", ch)
	}
	a := waitSent(t, w.alerts, 1)
	if len(a) != 1 || !strings.Contains(a[0].Msg.Markdown, "restored") {
		t.Errorf("sent %+v", a)
	}
}

func TestCheckHeartbeats_IndependentHeartbeats(t *testing.T) {
	w := newWorld(t, func(s *domain.Spec) {
		s.Heartbeats = append(s.Heartbeats, domain.Heartbeat{Name: "backup", Source: "backup", Timeout: time.Hour, To: []string{"critical"}})
	})
	w.in("hb", watchdog())
	w.clock.Advance(30 * time.Minute)
	ch := w.hb.Check()
	if len(ch) != 1 || ch[0].Name != "wd" {
		t.Errorf("only wd is overdue: %+v", ch)
	}
	w.clock.Advance(31 * time.Minute)
	if ch := w.hb.Check(); len(ch) != 1 || ch[0].Name != "backup" {
		t.Errorf("now backup: %+v", ch)
	}
}

func TestCheckHeartbeats_States(t *testing.T) {
	w := newWorld(t)
	w.clock.Advance(time.Minute)
	w.in("hb", watchdog())
	seen := w.clock.Now()
	st := w.hb.States()
	if len(st) != 1 || st[0].Name != "wd" || !st[0].LastSeen.Equal(seen) || st[0].Missing || st[0].Timeout != 10*time.Minute {
		t.Errorf("states %+v", st)
	}
}

type failingEnqueuer struct{ calls int }

func (f *failingEnqueuer) Enqueue(domain.Delivery) (<-chan error, error) {
	f.calls++
	return nil, ErrQueueFull
}

func TestCheckHeartbeats_QueueFailureDoesNotLoseState(t *testing.T) {
	w := newWorld(t)
	fe := &failingEnqueuer{}
	hb := NewCheckHeartbeats(w.spec.Heartbeats, w.coord, w.router, fe, w.clock, quiet())
	w.clock.Advance(11 * time.Minute)
	if ch := hb.Check(); len(ch) != 1 || fe.calls != 2 {
		t.Errorf("changes %+v, enqueue calls %d", ch, fe.calls)
	}
	if !hb.States()[0].Missing {
		t.Error("state must be kept even if the alert could not be queued")
	}
}

var _ ports.CheckHeartbeats = (*CheckHeartbeats)(nil)
