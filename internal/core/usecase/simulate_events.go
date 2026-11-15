package usecase

import (
	"sync/atomic"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// SimulateEvents implements ports.SimulateEvents. It runs the real router on
// the events but touches nothing: no heartbeat is recorded, no dedupe key
// claimed, nothing counted, nothing sent.
type SimulateEvents struct {
	routing  atomic.Pointer[routing]
	silences Silencer
}

var _ ports.SimulateEvents = (*SimulateEvents)(nil)

func NewSimulateEvents(router *domain.Router, heartbeats []domain.Heartbeat) *SimulateEvents {
	u := &SimulateEvents{}
	u.SetRouting(router, heartbeats)
	return u
}

// SetRouting switches to a reloaded configuration.
func (u *SimulateEvents) SetRouting(router *domain.Router, heartbeats []domain.Heartbeat) {
	u.routing.Store(&routing{router, heartbeats})
}

// WithSilences shows which events an active silence would mute.
func (u *SimulateEvents) WithSilences(s Silencer) *SimulateEvents {
	u.silences = s
	return u
}

func (u *SimulateEvents) Simulate(events []*domain.Event) []ports.Simulation {
	rt := u.routing.Load()
	out := make([]ports.Simulation, 0, len(events))
	for _, ev := range events {
		sim := ports.Simulation{Event: ev}
		if hb, ok := domain.FindHeartbeat(rt.heartbeats, ev); ok {
			sim.Heartbeat = hb.Name
			out = append(out, sim)
			continue
		}
		res, err := rt.router.Route(ev)
		if err != nil {
			sim.Err = err.Error()
		}
		sim.Trace, sim.Deliveries, sim.DroppedBy = res.Trace, res.Deliveries, res.DroppedBy
		if u.silences != nil && len(res.Deliveries) > 0 {
			if s, ok := u.silences.Find(ev); ok {
				sim.Silenced = silencedReason(s)
			}
		}
		out = append(out, sim)
	}
	return out
}
