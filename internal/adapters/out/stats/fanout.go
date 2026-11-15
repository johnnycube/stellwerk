package stats

import (
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// Fanout passes every record to several recorders, e.g. the in-memory stats
// for the UI and the Prometheus metrics.
type Fanout []ports.Recorder

var _ ports.Recorder = Fanout(nil)

func (f Fanout) RecordEvent(e ports.EventRecord) {
	for _, r := range f {
		r.RecordEvent(e)
	}
}

func (f Fanout) RecordDelivery(d domain.Delivery, err error) {
	for _, r := range f {
		r.RecordDelivery(d, err)
	}
}
