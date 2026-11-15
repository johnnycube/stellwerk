package ports_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func TestSystemClockContract(t *testing.T) {
	var clock ports.SystemClock
	portstest.ClockContract(t, clock)
	before := clock.Now()
	time.Sleep(time.Millisecond)
	if !clock.Now().After(before) {
		t.Error("system clock must move")
	}
}

func TestManualClockContract(t *testing.T) {
	c := portstest.NewClock()
	portstest.ClockContract(t, c)
	before := c.Now()
	c.Advance(time.Hour)
	if c.Now().Sub(before) != time.Hour {
		t.Error("manual clock must advance exactly")
	}
}

func TestOutcomeNames(t *testing.T) {
	want := []string{"nothing to do", "delivered", "accepted", "failed", "timed out", "busy", "broken"}
	for o := ports.NothingToDo; o <= ports.Broken; o++ {
		if o.String() != want[o] {
			t.Errorf("%d: %q", o, o.String())
		}
	}
}

// The shared audit rules live on ports.AuditEntry, run the contract here too.
func TestAuditRulesContract(t *testing.T) {
	portstest.AuditStoreContract(t, func() ports.AuditStore { return portstest.NewAuditStore() })
}

func TestAuditEntrySettled(t *testing.T) {
	if (ports.AuditEntry{Verdict: ports.AuditPending}).Settled() || !(ports.AuditEntry{Verdict: ports.AuditDropped}).Settled() {
		t.Error("only pending entries are unsettled")
	}
	if c := (ports.AuditEntry{}).Clone(); c.Labels != nil || c.Deliveries != nil {
		t.Error("cloning an empty entry stays empty")
	}
}

func TestErrorClassification(t *testing.T) {
	base := errors.New("boom")
	if ports.Permanent(nil) != nil || ports.RetryAfter(nil, time.Second) != nil {
		t.Error("nil stays nil")
	}
	p := fmt.Errorf("send: %w", ports.Permanent(base))
	if !ports.IsPermanent(p) || !errors.Is(p, base) || p.Error() != "send: boom" || ports.IsPermanent(base) {
		t.Error("permanent")
	}
	r := fmt.Errorf("x: %w", ports.RetryAfter(base, 3*time.Second))
	if d, ok := ports.RetryAfterOf(r); !ok || d != 3*time.Second || !errors.Is(r, base) || r.Error() != "x: boom" {
		t.Error("retry after")
	}
	if _, ok := ports.RetryAfterOf(base); ok {
		t.Error("plain error has no retry-after")
	}
	if _, ok := ports.RetryAfterOf(ports.RetryAfter(base, 0)); ok {
		t.Error("zero wait is no wait")
	}
}
