// Package scheduler is a driving adapter: it calls the CheckHeartbeats use
// case on a fixed interval, like a cron job would.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

type Heartbeats struct {
	UseCase  ports.CheckHeartbeats
	Interval time.Duration
	Log      *slog.Logger
}

// Run checks every Interval until ctx ends.
func (h *Heartbeats) Run(ctx context.Context) {
	t := time.NewTicker(h.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, c := range h.UseCase.Check() {
				if h.Log != nil {
					h.Log.Info("heartbeat changed", "heartbeat", c.Name, "missing", c.Missing, "alert_sent", c.Sent)
				}
			}
		}
	}
}
