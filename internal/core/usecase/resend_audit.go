package usecase

import (
	"fmt"
	"sync/atomic"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// ResendAudit implements ports.ResendAudit: the messages of an audit entry
// go out again, as they were rendered then, to the destinations as they are
// configured now (with their current fallbacks). The result is a new audit
// entry that points back to the old one.
type ResendAudit struct {
	store   ports.AuditStore
	out     Enqueuer
	router  atomic.Pointer[domain.Router]
	auditor *auditor
}

var _ ports.ResendAudit = (*ResendAudit)(nil)

func NewResendAudit(store ports.AuditStore, out Enqueuer, router *domain.Router, clock ports.Clock, node string) *ResendAudit {
	u := &ResendAudit{store: store, out: out, auditor: newAuditor(store, clock, node, AuditPolicy{})}
	u.router.Store(router)
	return u
}

// SetRouter switches to a reloaded configuration.
func (u *ResendAudit) SetRouter(r *domain.Router) { u.router.Store(r) }

func (u *ResendAudit) Resend(id string, onlyFailed bool) (string, error) {
	old, ok := u.store.Get(id)
	if !ok {
		return "", fmt.Errorf("audit entry %q: %w (it may have been pushed out of memory)", id, ErrNotFound)
	}
	router := u.router.Load()
	var ds []domain.Delivery
	for _, d := range old.Deliveries {
		if d.FallbackFor != nil || (onlyFailed && d.Status != "failed") {
			continue // fallbacks come again from the destination's config
		}
		if !router.HasDestination(d.Destination) {
			return "", fmt.Errorf("destination %q no longer exists", d.Destination)
		}
		msg := domain.Message{Markdown: d.Message, Loud: d.Loud, Priority: d.Priority, Source: old.Source, Route: d.Route}
		ds = append(ds, router.Delivery(d.Route, d.Destination, msg))
	}
	if len(ds) == 0 {
		if onlyFailed {
			return "", fmt.Errorf("nothing failed in %s", id)
		}
		return "", fmt.Errorf("%s sent nothing, there is nothing to send again", id)
	}
	e := u.auditor.newEntry(old.Source, "resend", old.Status, old.Labels, nil)
	e.ResentFrom, e.Reason = id, "sent again from the audit log"
	u.auditor.attach(&e, ds)
	u.store.Record(e)
	for i, d := range ds {
		if _, err := u.out.Enqueue(d); err != nil {
			u.store.Finish(e.ID, i, u.auditor.clock.Now(), err)
		}
	}
	return e.ID, nil
}
