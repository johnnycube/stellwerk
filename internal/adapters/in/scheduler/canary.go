package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// Canary runs the canary every Every, or daily at At ("09:00") in Loc.
type Canary struct {
	UseCase ports.RunCanary
	Every   time.Duration
	At      string // optional, wins over Every
	Loc     *time.Location
	Log     *slog.Logger
}

func (c *Canary) Run(ctx context.Context) {
	for {
		wait := time.Until(c.next(time.Now()))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			c.UseCase.Run(ctx)
		}
	}
}

// next is when the canary runs after now.
func (c *Canary) next(now time.Time) time.Time {
	if c.At == "" {
		return now.Add(c.Every)
	}
	at, err := time.Parse("15:04", c.At)
	if err != nil {
		return now.Add(c.Every) // config validates, this is a safety net
	}
	loc := c.Loc
	if loc == nil {
		loc = time.Local
	}
	n := now.In(loc)
	t := time.Date(n.Year(), n.Month(), n.Day(), at.Hour(), at.Minute(), 0, 0, loc)
	if !t.After(n) {
		t = time.Date(n.Year(), n.Month(), n.Day()+1, at.Hour(), at.Minute(), 0, 0, loc) // DST safe
	}
	return t
}
