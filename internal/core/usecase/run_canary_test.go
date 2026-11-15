package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// echo is a Receiver that "reads back" what a Sender sent, unless broken.
type echo struct {
	mu     sync.Mutex
	broken bool
	asked  []string
}

func (e *echo) Expect(address, marker string) func(context.Context) error {
	e.mu.Lock()
	e.asked = append(e.asked, address+"|"+marker)
	broken := e.broken
	e.mu.Unlock()
	return func(ctx context.Context) error {
		if broken {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
}

func canaryWorld(t *testing.T, fallback bool) *world {
	return newWorld(t, func(s *domain.Spec) {
		if fallback {
			withFallback(s)
		}
	})
}

func TestCanary_Arrives(t *testing.T) {
	w := canaryWorld(t, false)
	rx := &echo{}
	c := NewRunCanary(CanarySpec{To: []string{"critical", "media"}, Timeout: time.Second}, w.router, w.deliver, rx, w.clock, quiet()).WithAudit(w.audit, "node-a")
	if _, ok := c.Last(); ok {
		t.Error("no run yet")
	}
	res := c.Run(context.Background())
	if !res.OK || !strings.HasPrefix(res.Marker, "canary-") || len(rx.asked) != 2 || rx.asked[0] != "!crit:x|"+res.Marker {
		t.Fatalf("%+v %v", res, rx.asked)
	}
	m := w.alerts.Sent()[0].Msg
	if !strings.Contains(m.Markdown, res.Marker) || m.Loud || m.Priority != domain.PriorityLow {
		t.Errorf("%+v", m)
	}
	if last, ok := c.Last(); !ok || last.Marker != res.Marker {
		t.Error("last")
	}
	eventually(t, "audit", func() bool { return w.last().Settled() })
	if e := w.last(); e.Kind != "canary" || e.Verdict != ports.AuditDelivered {
		t.Errorf("%+v", e)
	}
}

func TestCanary_UndeliverableGoesToTheFallback(t *testing.T) {
	w := canaryWorld(t, true)
	w.alerts.FailAll(true)
	c := NewRunCanary(CanarySpec{To: []string{"critical"}, Timeout: time.Second}, w.router, w.deliver, nil, w.clock, quiet())
	res := c.Run(context.Background())
	if res.OK || !strings.Contains(res.Problems[0], "critical") {
		t.Errorf("arriving only through the fallback is a failure: %+v", res)
	}
	eventually(t, "fallback got the canary", func() bool { return len(w.media.Sent()) == 1 })
	if m := w.media.Sent()[0].Msg; !strings.Contains(m.Markdown, "Canary") || !strings.Contains(m.Markdown, "could not deliver to `critical`") || !m.Loud {
		t.Errorf("%+v", m)
	}
}

func TestCanary_NotReadBackRaisesAlarm(t *testing.T) {
	w := canaryWorld(t, true)
	rx := &echo{broken: true}
	c := NewRunCanary(CanarySpec{To: []string{"critical"}, Timeout: 50 * time.Millisecond}, w.router, w.deliver, rx, w.clock, quiet())
	res := c.Run(context.Background())
	if res.OK || !strings.Contains(res.Problems[0], "not received back") {
		t.Fatalf("%+v", res)
	}
	eventually(t, "alarm to the fallback", func() bool { return len(w.media.Sent()) == 1 })
	if m := w.media.Sent()[0].Msg; !strings.Contains(m.Markdown, "Canary to `critical` failed") || !m.Loud {
		t.Errorf("%+v", m)
	}
}

func TestCanary_AlertDestinationAndNowhere(t *testing.T) {
	w := canaryWorld(t, false)
	w.alerts.FailAll(true)
	c := NewRunCanary(CanarySpec{To: []string{"critical"}, Alert: []string{"pager"}, Timeout: time.Second}, w.router, w.deliver, nil, w.clock, quiet())
	if res := c.Run(context.Background()); res.OK {
		t.Fatal("must fail")
	}
	eventually(t, "alert", func() bool { return len(w.hook.Sent()) == 1 })
	c2 := NewRunCanary(CanarySpec{To: []string{"critical"}, Timeout: time.Second}, w.router, refusing{}, nil, w.clock, quiet())
	if res := c2.Run(context.Background()); res.OK || !strings.Contains(res.Problems[0], "full") {
		t.Errorf("%+v", res)
	}
	_ = errors.New
}
