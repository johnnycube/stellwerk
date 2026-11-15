package stats

import (
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func TestMemoryRecorderContract(t *testing.T) {
	portstest.RecorderContract(t, func() (ports.Recorder, ports.StatsReader) { m := NewMemory(100); return m, m })
}

func TestMemoryKeepsOnlyTheLastEvents(t *testing.T) {
	m := NewMemory(3)
	for i := 0; i < 5; i++ {
		m.RecordEvent(ports.EventRecord{Time: time.Unix(int64(i), 0), Source: "s"})
	}
	s := m.Snapshot()
	if len(s.Events) != 3 || s.Events[0].Time.Unix() != 4 || s.Sources["s"].Received != 5 {
		t.Errorf("ring buffer: %+v", s)
	}
}

func TestFanoutReachesEveryRecorder(t *testing.T) {
	a, b := NewMemory(10), NewMemory(10)
	f := Fanout{a, b}
	f.RecordEvent(ports.EventRecord{Source: "s", Routes: []string{"r"}})
	f.RecordDelivery(domain.Delivery{Route: "r", Destination: "d"}, nil)
	for _, m := range []*Memory{a, b} {
		s := m.Snapshot()
		if s.Sources["s"].Received != 1 || s.Destinations["d"].Delivered != 1 {
			t.Errorf("%+v", s)
		}
	}
}

// A Fanout over a memory recorder still honours the Recorder contract.
func TestFanoutContract(t *testing.T) {
	portstest.RecorderContract(t, func() (ports.Recorder, ports.StatsReader) {
		m := NewMemory(100)
		return Fanout{m, NewMemory(1)}, m
	})
}
