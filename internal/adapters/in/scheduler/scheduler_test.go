package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

type countingCheck struct{ n atomic.Int32 }

func (c *countingCheck) Check() []ports.HeartbeatChange {
	c.n.Add(1)
	return []ports.HeartbeatChange{{Name: "wd", Missing: true, Sent: true}}
}
func (c *countingCheck) States() []ports.HeartbeatState { return nil }

func TestSchedulerDrivesTheUseCase(t *testing.T) {
	uc := &countingCheck{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&Heartbeats{UseCase: uc, Interval: 5 * time.Millisecond}).Run(ctx); close(done) }()
	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done
	if uc.n.Load() < 3 {
		t.Errorf("checked %d times", uc.n.Load())
	}
}
