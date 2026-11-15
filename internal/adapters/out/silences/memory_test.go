package silences

import (
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func TestMemoryContract(t *testing.T) {
	stores := map[*testing.T]*Memory{}
	portstest.SilenceStoreContract(t, func(t *testing.T) ports.SilenceStore {
		if s, ok := stores[t]; ok {
			return s
		}
		stores[t] = NewMemory()
		return stores[t]
	})
}
