// Package stats is an in-memory Recorder and StatsReader. Nothing is
// persisted and no message content is kept, only metadata.
package stats

import (
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

type Memory struct {
	mu      sync.Mutex
	sources map[string]*ports.SourceStats
	routes  map[string]*ports.RouteStats
	dests   map[string]*ports.DestStats
	events  []ports.EventRecord
	max     int
}

var (
	_ ports.Recorder    = (*Memory)(nil)
	_ ports.StatsReader = (*Memory)(nil)
)

func NewMemory(maxEvents int) *Memory {
	return &Memory{sources: map[string]*ports.SourceStats{}, routes: map[string]*ports.RouteStats{}, dests: map[string]*ports.DestStats{}, max: maxEvents}
}

func (m *Memory) RecordEvent(rec ports.EventRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.sources[rec.Source]
	if src == nil {
		src = &ports.SourceStats{}
		m.sources[rec.Source] = src
	}
	src.Received++
	src.LastSeen = rec.Time
	for _, name := range rec.Routes {
		rs := m.route(name)
		rs.Matched++
		rs.LastMatch = rec.Time
	}
	m.events = append(m.events, rec)
	if len(m.events) > m.max {
		m.events = m.events[len(m.events)-m.max:]
	}
}

func (m *Memory) RecordDelivery(d domain.Delivery, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.route(d.Route)
	ds := m.dests[d.Destination]
	if ds == nil {
		ds = &ports.DestStats{}
		m.dests[d.Destination] = ds
	}
	if err != nil {
		rs.Failed++
		rs.LastError = err.Error()
		ds.Failed++
		return
	}
	rs.Delivered++
	ds.Delivered++
	ds.LastSent = time.Now()
}

func (m *Memory) route(name string) *ports.RouteStats {
	rs := m.routes[name]
	if rs == nil {
		rs = &ports.RouteStats{}
		m.routes[name] = rs
	}
	return rs
}

func (m *Memory) Snapshot() ports.StatsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := ports.StatsSnapshot{
		Sources:      map[string]ports.SourceStats{},
		Routes:       map[string]ports.RouteStats{},
		Destinations: map[string]ports.DestStats{},
		Events:       make([]ports.EventRecord, len(m.events)),
	}
	for k, v := range m.sources {
		snap.Sources[k] = *v
	}
	for k, v := range m.routes {
		snap.Routes[k] = *v
	}
	for k, v := range m.dests {
		snap.Destinations[k] = *v
	}
	for i, e := range m.events { // newest first
		snap.Events[len(m.events)-1-i] = e
	}
	return snap
}
