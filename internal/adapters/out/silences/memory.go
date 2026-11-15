// Package silences holds the in-memory silence store, used when no spool
// file is configured. Silences then end with a restart. With
// delivery.spool, the spool package keeps them in the same SQLite file.
package silences

import (
	"sort"
	"sync"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

type Memory struct {
	mu   sync.Mutex
	byID map[string]domain.Silence
}

var _ ports.SilenceStore = (*Memory)(nil)

func NewMemory() *Memory { return &Memory{byID: map[string]domain.Silence{}} }

func (m *Memory) Save(s domain.Silence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[s.ID] = s
	return nil
}

func (m *Memory) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byID, id)
	return nil
}

func (m *Memory) All() ([]domain.Silence, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Silence, 0, len(m.byID))
	for _, s := range m.byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
