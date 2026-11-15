package usecase

import (
	"sort"
	"sync/atomic"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// DescribeTopology implements ports.DescribeTopology: the configured rules,
// destinations, templates and heartbeats, plus live state from the
// coordinator, the heartbeat check and the stats.
type DescribeTopology struct {
	cur        atomic.Pointer[describeGen]
	silences   ports.ManageSilences
	heartbeats ports.CheckHeartbeats
	coord      ports.Coordinator
	stats      ports.StatsReader
}

var _ ports.DescribeTopology = (*DescribeTopology)(nil)

func NewDescribeTopology(spec domain.Spec, renderer *domain.Renderer, hb ports.CheckHeartbeats, coord ports.Coordinator, stats ports.StatsReader) *DescribeTopology {
	u := &DescribeTopology{heartbeats: hb, coord: coord, stats: stats}
	u.SetSpec(spec, renderer)
	return u
}

type describeGen struct {
	spec     domain.Spec
	renderer *domain.Renderer
}

// SetSpec switches to a reloaded configuration.
func (u *DescribeTopology) SetSpec(spec domain.Spec, renderer *domain.Renderer) {
	u.cur.Store(&describeGen{spec, renderer})
}

// WithSilences lists active silences in the topology.
func (u *DescribeTopology) WithSilences(s ports.ManageSilences) *DescribeTopology {
	u.silences = s
	return u
}

func (u *DescribeTopology) Describe() ports.Topology {
	g := u.cur.Load()
	t := ports.Topology{Engine: g.spec.Engine, Rules: g.spec.Rules, Cluster: u.coord.Status()}
	if u.silences != nil {
		t.Silences = u.silences.Active()
	}
	for _, d := range g.spec.Destinations {
		t.Destinations = append(t.Destinations, d)
	}
	sort.Slice(t.Destinations, func(i, j int) bool { return t.Destinations[i].Alias < t.Destinations[j].Alias })
	if u.heartbeats != nil {
		t.Heartbeats = u.heartbeats.States()
	}
	for name, tm := range g.renderer.Sources() {
		t.Templates = append(t.Templates, ports.Template{Name: name, Source: tm.Source, Builtin: tm.Builtin})
	}
	sort.Slice(t.Templates, func(i, j int) bool { return t.Templates[i].Name < t.Templates[j].Name })
	if u.stats != nil {
		t.Stats = u.stats.Snapshot()
	}
	return t
}
