package local

import (
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func TestLocalCoordinatorContract(t *testing.T) {
	portstest.CoordinatorContract(t, func(t *testing.T) portstest.CoordinatorUnderTest {
		return portstest.CoordinatorUnderTest{Coordinator: New("solo"), Advance: time.Sleep}
	})
}

func TestLocalIsAlwaysLeaderWithoutHA(t *testing.T) {
	c := New("solo")
	if !c.IsLeader() || c.Status().Enabled || c.Status().Leader != "solo" {
		t.Errorf("%+v", c.Status())
	}
}
