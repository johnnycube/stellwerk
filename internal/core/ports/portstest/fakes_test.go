package portstest

import (
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// The fakes must honour the same contracts as the real adapters.

func TestFakeCoordinatorContract(t *testing.T) {
	CoordinatorContract(t, func(t *testing.T) CoordinatorUnderTest {
		clock := NewClock()
		return CoordinatorUnderTest{Coordinator: NewCoordinator("n1", clock), Advance: clock.Advance}
	})
}

func TestFakeRecorderContract(t *testing.T) {
	RecorderContract(t, func() (ports.Recorder, ports.StatsReader) { r := NewRecorder(); return r, r })
}

func TestFakeSenderContract(t *testing.T) {
	SenderContract(t, func(t *testing.T) SenderUnderTest {
		s := NewSender()
		return SenderUnderTest{Sender: s, Received: s.Sent, Break: func() { s.FailAll(true) }}
	})
}

func TestFakeAuditStoreContract(t *testing.T) {
	AuditStoreContract(t, func() ports.AuditStore { return NewAuditStore() })
}

func TestFakeOutboxContract(t *testing.T) {
	stores := map[*testing.T]*Outbox{}
	OutboxContract(t, func(t *testing.T) ports.Outbox {
		if o, ok := stores[t]; ok {
			return o // "reopen": the same memory
		}
		stores[t] = NewOutbox()
		return stores[t]
	})
}

func TestFakeSilenceStoreContract(t *testing.T) {
	stores := map[*testing.T]*SilenceStore{}
	SilenceStoreContract(t, func(t *testing.T) ports.SilenceStore {
		if s, ok := stores[t]; ok {
			return s
		}
		stores[t] = NewSilenceStore()
		return stores[t]
	})
}
