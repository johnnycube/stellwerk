// Package audit stores audit entries: a bounded in-memory log for the UI,
// plus optional sinks that receive every entry once it is settled (all its
// deliveries finished), such as a JSON lines file or the application log.
package audit

import (
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// Sink receives settled entries. It must not block for long.
type Sink interface {
	Write(ports.AuditEntry)
}

// Memory keeps the newest Size entries.
type Memory struct {
	mu      sync.Mutex
	size    int
	ring    []*ports.AuditEntry // oldest first
	byID    map[string]*ports.AuditEntry
	sinks   []Sink
	evicted int
}

var _ ports.AuditStore = (*Memory)(nil)

func NewMemory(size int, sinks ...Sink) *Memory {
	if size <= 0 {
		size = 1000
	}
	return &Memory{size: size, byID: map[string]*ports.AuditEntry{}, sinks: sinks}
}

// AddSink adds a sink before use. Not safe while entries are recorded.
func (m *Memory) AddSink(s Sink) { m.sinks = append(m.sinks, s) }

func (m *Memory) Record(e ports.AuditEntry) {
	c := e.Clone()
	m.mu.Lock()
	if len(m.ring) >= m.size {
		old := m.ring[0]
		delete(m.byID, old.ID)
		m.ring = m.ring[1:]
		m.evicted++
	}
	m.ring = append(m.ring, &c)
	m.byID[c.ID] = &c
	m.mu.Unlock()
	if c.Settled() {
		m.emit(c)
	}
}

func (m *Memory) Attempt(id string, i int, a ports.AuditAttempt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byID[id]; ok {
		e.AddAttempt(i, a)
	}
}

func (m *Memory) AddDelivery(id string, d ports.AuditDelivery) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byID[id]; ok {
		return e.AddDelivery(d)
	}
	return -1
}

func (m *Memory) Finish(id string, i int, at time.Time, err error) {
	m.mu.Lock()
	e, ok := m.byID[id]
	settled := ok && e.FinishDelivery(i, at, err)
	var c ports.AuditEntry
	if settled {
		c = e.Clone()
	}
	m.mu.Unlock()
	if settled {
		m.emit(c)
	}
}

func (m *Memory) emit(e ports.AuditEntry) {
	for _, s := range m.sinks {
		s.Write(e)
	}
}

func (m *Memory) List(q ports.AuditQuery) []ports.AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ports.AuditEntry
	for i := len(m.ring) - 1; i >= 0 && (q.Limit <= 0 || len(out) < q.Limit); i-- {
		if m.ring[i].Matches(q) {
			out = append(out, m.ring[i].Clone())
		}
	}
	return out
}

func (m *Memory) Get(id string) (ports.AuditEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byID[id]
	if !ok {
		return ports.AuditEntry{}, false
	}
	return e.Clone(), true
}

// Len and Evicted are for metrics.
func (m *Memory) Len() int     { m.mu.Lock(); defer m.mu.Unlock(); return len(m.ring) }
func (m *Memory) Evicted() int { m.mu.Lock(); defer m.mu.Unlock(); return m.evicted }
