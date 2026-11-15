package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

func TestCanaryNext(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	c := &Canary{At: "09:00", Loc: berlin}
	morning := time.Date(2026, 11, 15, 8, 0, 0, 0, berlin)
	if got := c.next(morning); !got.Equal(time.Date(2026, 11, 15, 9, 0, 0, 0, berlin)) {
		t.Errorf("same day: %v", got)
	}
	if got := c.next(time.Date(2026, 11, 15, 9, 0, 0, 0, berlin)); !got.Equal(time.Date(2026, 11, 16, 9, 0, 0, 0, berlin)) {
		t.Errorf("next day: %v", got)
	}
	dst := time.Date(2027, 3, 27, 10, 0, 0, 0, berlin) // clocks go forward that night
	if got := c.next(dst); got.Hour() != 9 || got.Day() != 28 {
		t.Errorf("dst: %v", got)
	}
	e := &Canary{Every: time.Hour}
	if got := e.next(morning); !got.Equal(morning.Add(time.Hour)) {
		t.Errorf("every: %v", got)
	}
	bad := &Canary{At: "nine", Every: time.Minute}
	if got := bad.next(morning); !got.Equal(morning.Add(time.Minute)) {
		t.Error("bad at falls back to every")
	}
	if (&Canary{At: "23:59"}).next(morning).Location() != time.Local {
		t.Error("default location")
	}
}

type countCanary struct{ n atomic.Int32 }

func (c *countCanary) Run(context.Context) ports.CanaryResult {
	c.n.Add(1)
	return ports.CanaryResult{OK: true}
}
func (c *countCanary) Last() (ports.CanaryResult, bool) { return ports.CanaryResult{}, false }

func TestCanaryRun(t *testing.T) {
	uc := &countCanary{}
	ctx, cancel := context.WithCancel(context.Background())
	go (&Canary{UseCase: uc, Every: 10 * time.Millisecond}).Run(ctx)
	time.Sleep(55 * time.Millisecond)
	cancel()
	if n := uc.n.Load(); n < 3 {
		t.Errorf("ran %d times", n)
	}
}
