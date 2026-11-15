package usecase

import (
	"fmt"
	"sync/atomic"
	"unicode/utf8"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// AuditPolicy says what goes into audit entries.
type AuditPolicy struct {
	Payload    bool // keep request bodies
	PayloadMax int  // bytes, longer bodies are cut
}

// auditor builds audit entries. It is shared by the use cases that create
// them, so IDs are unique per node and ordered by time.
type auditor struct {
	store  ports.AuditStore
	clock  ports.Clock
	node   string
	policy AuditPolicy
}

// auditSeq is shared by all auditors of the process, so two use cases that
// create entries in the same millisecond never get the same ID.
var auditSeq atomic.Uint64

func newAuditor(store ports.AuditStore, clock ports.Clock, node string, p AuditPolicy) *auditor {
	if p.PayloadMax <= 0 {
		p.PayloadMax = 16 << 10
	}
	return &auditor{store: store, clock: clock, node: node, policy: p}
}

func (a *auditor) enabled() bool { return a != nil && a.store != nil }

func (a *auditor) newEntry(source, kind, status string, labels map[string]string, raw []byte) ports.AuditEntry {
	now := a.clock.Now()
	e := ports.AuditEntry{
		ID:   fmt.Sprintf("%s-%013x-%04x", a.node, now.UnixMilli(), auditSeq.Add(1)&0xffff),
		Time: now, Node: a.node, Source: source, Kind: kind, Status: status, Labels: labels,
	}
	if a.policy.Payload && len(raw) > 0 {
		e.Payload, e.PayloadTruncated = cut(raw, a.policy.PayloadMax)
	}
	return e
}

// attach adds deliveries to the entry and links them to it.
func (a *auditor) attach(e *ports.AuditEntry, ds []domain.Delivery) {
	for i := range ds {
		ds[i].EntryID, ds[i].Index = e.ID, len(e.Deliveries)
		e.Deliveries = append(e.Deliveries, auditDelivery(ds[i]))
	}
	if len(ds) > 0 {
		e.Verdict = ports.AuditPending
	}
}

func auditDelivery(d domain.Delivery) ports.AuditDelivery {
	a := ports.AuditDelivery{
		Route: d.Route, Destination: d.Destination, Endpoint: d.Endpoint, Address: d.Address,
		Loud: d.Message.Loud, Priority: d.Message.Priority, Message: d.Message.Markdown, Status: "pending",
	}
	if d.Digest > 0 {
		a.Digest = d.Digest.String()
	}
	return a
}

func cut(b []byte, limit int) (string, bool) {
	if len(b) <= limit {
		return string(b), false
	}
	b = b[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b), true
}

// ReadAudit implements ports.ReadAudit.
type ReadAudit struct {
	store ports.AuditStore
}

var _ ports.ReadAudit = (*ReadAudit)(nil)

func NewReadAudit(store ports.AuditStore) *ReadAudit { return &ReadAudit{store: store} }

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
)

func (u *ReadAudit) List(q ports.AuditQuery) []ports.AuditEntry {
	if u.store == nil {
		return nil
	}
	switch {
	case q.Limit <= 0:
		q.Limit = defaultAuditLimit
	case q.Limit > maxAuditLimit:
		q.Limit = maxAuditLimit
	}
	return u.store.List(q)
}

func (u *ReadAudit) Get(id string) (ports.AuditEntry, bool) {
	if u.store == nil {
		return ports.AuditEntry{}, false
	}
	return u.store.Get(id)
}
