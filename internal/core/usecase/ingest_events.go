package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// IngestEvents implements ports.IngestEvents. Per event, in this order:
//
//  1. heartbeat: record it on the coordinator, done
//  2. duplicate: if dedupe is on and the payload was delivered before, done
//  3. route: drop, no match, or deliveries
//  4. deliver: in sync mode wait for the result, in async mode just queue
//
// A payload counts as seen only after it was delivered (sync) or queued
// (async), so a failed delivery can be retried by the sender.
type IngestEvents struct {
	routing  atomic.Pointer[routing]
	silences Silencer
	out      Enqueuer
	coord    ports.Coordinator
	rec      ports.Recorder
	clock    ports.Clock
	policy   domain.DeliveryPolicy
	log      *slog.Logger
	audit    *auditor
}

var _ ports.IngestEvents = (*IngestEvents)(nil)

func NewIngestEvents(router *domain.Router, heartbeats []domain.Heartbeat, out Enqueuer, coord ports.Coordinator,
	rec ports.Recorder, clock ports.Clock, p domain.DeliveryPolicy, log *slog.Logger,
) *IngestEvents {
	u := &IngestEvents{out: out, coord: coord, rec: rec, clock: clock, policy: p, log: log}
	u.SetRouting(router, heartbeats)
	return u
}

// routing is one generation of rules and heartbeats. A reload swaps it
// whole, so an event is always routed by one consistent configuration.
type routing struct {
	router     *domain.Router
	heartbeats []domain.Heartbeat
}

// SetRouting switches to a reloaded configuration.
func (u *IngestEvents) SetRouting(router *domain.Router, heartbeats []domain.Heartbeat) {
	u.routing.Store(&routing{router, heartbeats})
}

// WithSilences mutes events that match an active silence.
func (u *IngestEvents) WithSilences(s Silencer) *IngestEvents {
	u.silences = s
	return u
}

// WithAudit turns on audit entries for every event.
func (u *IngestEvents) WithAudit(store ports.AuditStore, p AuditPolicy) *IngestEvents {
	u.audit = newAuditor(store, u.clock, u.coord.NodeID(), p)
	return u
}

func (u *IngestEvents) Refuse(req ports.RefusedRequest) {
	if !u.audit.enabled() {
		return
	}
	e := u.audit.newEntry(req.Source, req.Kind, "", nil, req.Raw)
	e.Verdict, e.Reason = ports.AuditRejected, req.Reason
	u.audit.store.Record(e)
}

type pending struct {
	ev     *domain.Event
	res    domain.Result
	dedupe string
	done   []<-chan error
}

func (u *IngestEvents) Ingest(ctx context.Context, req ports.IngestRequest) ports.IngestResult {
	rt := u.routing.Load()
	var work []pending
	for i, ev := range req.Events {
		var entry ports.AuditEntry
		if u.audit.enabled() {
			entry = u.audit.newEntry(ev.Source, ev.Kind, ev.Status, ev.Labels, req.Raw)
		}
		if hb, ok := domain.FindHeartbeat(rt.heartbeats, ev); ok {
			u.coord.SetHeartbeat(hb.Name, u.clock.Now())
			u.record(ev, domain.Result{}, "heartbeat")
			entry.Verdict, entry.Heartbeat = ports.AuditHeartbeat, hb.Name
			u.store(entry)
			continue
		}
		p := pending{ev: ev}
		if u.policy.DedupeWindow > 0 {
			p.dedupe = DedupeKey(ev, req.Raw, i)
			if u.coord.Seen(p.dedupe) {
				u.record(ev, domain.Result{}, "duplicate")
				entry.Verdict, entry.Reason = ports.AuditDuplicate, "same payload delivered within the dedupe window"
				u.store(entry)
				continue
			}
		}
		res, err := rt.router.Route(ev)
		entry.Trace, entry.DroppedBy = res.Trace, res.DroppedBy
		if err != nil {
			u.record(ev, res, "error")
			u.log.Error("routing failed", "source", ev.Source, "err", err)
			entry.Verdict, entry.Reason = ports.AuditError, err.Error()
			u.store(entry)
			return ports.IngestResult{Outcome: ports.Broken, Detail: err.Error()}
		}
		p.res = res
		if len(res.Deliveries) == 0 {
			if res.DroppedBy != "" {
				u.record(ev, res, "dropped")
				entry.Verdict = ports.AuditDropped
			} else {
				u.record(ev, res, "unmatched")
				entry.Verdict = ports.AuditUnmatched
			}
			u.store(entry)
			continue
		}
		if u.silences != nil {
			if s, ok := u.silences.Find(ev); ok {
				u.record(ev, res, "silenced")
				entry.Verdict, entry.SilencedBy = ports.AuditSilenced, s.ID
				entry.Reason = silencedReason(s)
				u.store(entry)
				continue
			}
		}
		if u.audit.enabled() {
			u.audit.attach(&entry, res.Deliveries) // links deliveries to the entry
			u.audit.store.Record(entry)            // before enqueueing, so attempts find it
		}
		for j, d := range res.Deliveries {
			done, err := u.out.Enqueue(d)
			if err != nil {
				u.record(ev, res, "rejected")
				if u.audit.enabled() {
					for k := j; k < len(res.Deliveries); k++ {
						u.audit.store.Finish(entry.ID, k, u.clock.Now(), err)
					}
				}
				return ports.IngestResult{Outcome: ports.Busy, Detail: err.Error()}
			}
			p.done = append(p.done, done)
		}
		work = append(work, p)
	}
	if len(work) == 0 {
		return ports.IngestResult{Outcome: ports.NothingToDo}
	}

	if !u.policy.Sync {
		for _, p := range work {
			u.claim(p.dedupe)
			u.record(p.ev, p.res, "queued")
		}
		return ports.IngestResult{Outcome: ports.Accepted}
	}

	timeout := time.NewTimer(u.policy.Timeout)
	defer timeout.Stop()
	var failed []string
	for _, p := range work {
		ok := true
		for _, done := range p.done {
			select {
			case err := <-done:
				if err != nil {
					ok = false
					failed = append(failed, err.Error())
				}
			case <-timeout.C:
				u.record(p.ev, p.res, "failed")
				return ports.IngestResult{Outcome: ports.TimedOut, Detail: "delivery timed out"}
			case <-ctx.Done():
				return ports.IngestResult{Outcome: ports.TimedOut, Detail: "request cancelled"}
			}
		}
		if ok {
			u.claim(p.dedupe)
			u.record(p.ev, p.res, "delivered")
		} else {
			u.record(p.ev, p.res, "failed")
		}
	}
	if len(failed) > 0 {
		return ports.IngestResult{Outcome: ports.Failed, Detail: strings.Join(failed, "; ")}
	}
	return ports.IngestResult{Outcome: ports.Delivered}
}

func silencedReason(s domain.Silence) string {
	r := fmt.Sprintf("silenced by %s (%s) until %s", s.ID, s.Describe(), s.Ends.Format("2006-01-02 15:04 MST"))
	if s.Comment != "" {
		r += ": " + s.Comment
	}
	return r
}

// store records a settled entry if auditing is on.
func (u *IngestEvents) store(e ports.AuditEntry) {
	if u.audit.enabled() {
		u.audit.store.Record(e)
	}
}

func (u *IngestEvents) claim(key string) {
	if key != "" {
		u.coord.Claim(key, u.policy.DedupeWindow)
	}
}

func (u *IngestEvents) record(ev *domain.Event, res domain.Result, verdict string) {
	if u.rec == nil {
		return
	}
	var routes []string
	for _, d := range res.Deliveries {
		if len(routes) == 0 || routes[len(routes)-1] != d.Route {
			routes = append(routes, d.Route)
		}
	}
	if res.DroppedBy != "" {
		routes = append(routes, res.DroppedBy)
	}
	u.rec.RecordEvent(ports.EventRecord{
		Time: u.clock.Now(), Node: u.coord.NodeID(), Source: ev.Source, Status: ev.Status,
		Routes: routes, Dropped: res.DroppedBy, Verdict: verdict,
	})
}

// DedupeKey identifies a payload: the event's own key if it has one,
// otherwise a hash of source, position and raw body.
func DedupeKey(ev *domain.Event, raw []byte, i int) string {
	if ev.Key != "" {
		return ev.Source + ":" + ev.Key
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00", ev.Source, i)
	h.Write(raw)
	return ev.Source + ":" + hex.EncodeToString(h.Sum(nil))[:32]
}
