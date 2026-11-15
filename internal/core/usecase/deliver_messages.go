// Package usecase holds the application's use cases. Each one implements a
// driving port from internal/core/ports and is the only way to do that thing:
// adapters call use cases, never the domain or other adapters directly.
//
//	IngestEvents      route and deliver what a sender posted
//	SimulateEvents    show what would happen, without side effects
//	DescribeTopology  the whole setup and its live state
//	CheckHeartbeats   alert when a heartbeat stops or comes back
//	DeliverMessages   spool, queue, send, retry and fall back, per endpoint
package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

var (
	ErrQueueFull       = errors.New("queue full, retry later")
	ErrUnknownEndpoint = errors.New("no sender for endpoint")
	ErrSpool           = errors.New("spool")
	// ErrInterrupted: the delivery stopped because stellwerk shut down. It
	// stays in the spool (if there is one) and is sent after the restart.
	ErrInterrupted = errors.New("interrupted by shutdown")
	// ErrExpired: the message was older than delivery.max_age, not sent.
	ErrExpired = errors.New("too old, not sent")
	// ErrBreakerOpen: the endpoint failed too often in a row and is skipped
	// for a while, so fallbacks get the message without waiting.
	ErrBreakerOpen = errors.New("endpoint keeps failing, skipped for now")
)

// maxRetryAfter caps how long a backend may ask us to wait.
const maxRetryAfter = 10 * time.Minute

// Enqueuer is what other use cases need from DeliverMessages.
type Enqueuer interface {
	Enqueue(domain.Delivery) (<-chan error, error)
}

type job struct {
	d    domain.Delivery
	done chan error
}

// DeliverMessages queues deliveries per endpoint (target account), so a slow
// or broken account does not hold up the others. One worker per endpoint
// keeps order. Failed sends are retried with exponential backoff up to
// MaxRetries attempts. When the last attempt fails, the destination's
// fallbacks get the message.
//
// With an Outbox, a delivery is durable before Enqueue returns and is only
// removed once finished, so deliveries survive a restart (see Run).
type DeliverMessages struct {
	queues     map[string]chan job
	senders    map[string]ports.Sender
	maxRetries int
	maxBackoff time.Duration
	rec        ports.Recorder
	log        *slog.Logger
	pending    atomic.Int64 // queued or being sent
	audit      ports.AuditStore
	auditor    *auditor
	clock      ports.Clock
	outbox     ports.Outbox
	resume     []domain.Delivery // left in the outbox by the previous run
	policy     domain.DeliveryPolicy
	breakers   map[string]*breaker
}

// breaker counts deliveries to one endpoint that finally failed in a row.
type breaker struct {
	mu        sync.Mutex
	fails     int
	openUntil time.Time
}

// BreakerState is what the metrics and the UI show per endpoint.
type BreakerState struct {
	Failures  int
	OpenUntil time.Time // zero or past: closed
}

var _ Enqueuer = (*DeliverMessages)(nil)

func NewDeliverMessages(senders map[string]ports.Sender, p domain.DeliveryPolicy, rec ports.Recorder, log *slog.Logger) *DeliverMessages {
	d := &DeliverMessages{
		queues: map[string]chan job{}, senders: senders, maxRetries: max(p.MaxRetries, 1), maxBackoff: p.MaxBackoff,
		rec: rec, log: log, clock: ports.SystemClock{}, policy: p, breakers: map[string]*breaker{},
	}
	if d.maxBackoff <= 0 {
		d.maxBackoff = time.Minute
	}
	for ep := range senders {
		d.queues[ep] = make(chan job, max(p.QueueSize, 1))
		d.breakers[ep] = &breaker{}
	}
	return d
}

// Breakers returns the circuit breaker state per endpoint.
func (d *DeliverMessages) Breakers() map[string]BreakerState {
	out := make(map[string]BreakerState, len(d.breakers))
	for ep, b := range d.breakers {
		b.mu.Lock()
		out[ep] = BreakerState{Failures: b.fails, OpenUntil: b.openUntil}
		b.mu.Unlock()
	}
	return out
}

// WithAudit records every send attempt and the final result in the audit
// log. node names this node in entries for deliveries recovered from the spool.
func (d *DeliverMessages) WithAudit(store ports.AuditStore, clock ports.Clock, node string) *DeliverMessages {
	d.audit, d.clock = store, clock
	d.auditor = newAuditor(store, clock, node, AuditPolicy{})
	return d
}

// WithOutbox makes deliveries durable. What the outbox still holds from the
// previous run is read now, before any new delivery is accepted, and sent
// once Run starts.
func (d *DeliverMessages) WithOutbox(o ports.Outbox) *DeliverMessages {
	d.outbox = o
	left, err := o.Pending()
	if err != nil {
		d.log.Error("spool unreadable, deliveries from before the restart are not sent", "err", err)
	}
	d.resume = left
	return d
}

// Enqueue returns a channel that receives the final result: nil if the
// destination or one of its fallbacks got the message.
func (d *DeliverMessages) Enqueue(del domain.Delivery) (<-chan error, error) {
	q, ok := d.queues[del.Endpoint]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownEndpoint, del.Endpoint)
	}
	if del.ID == "" {
		del.ID = newID()
	}
	if del.Created.IsZero() {
		del.Created = d.clock.Now()
	}
	if d.outbox != nil {
		if err := d.outbox.Put(del); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpool, err)
		}
	}
	j := job{d: del, done: make(chan error, 1)}
	d.pending.Add(1)
	select {
	case q <- j:
		return j.done, nil
	default:
		d.pending.Add(-1)
		d.forget(del.ID)
		return nil, ErrQueueFull
	}
}

// HasEndpoint reports whether there is a sender for endpoint.
func (d *DeliverMessages) HasEndpoint(ep string) bool { _, ok := d.queues[ep]; return ok }

// Pending is the number of deliveries queued or being sent.
func (d *DeliverMessages) Pending() int64 { return d.pending.Load() }

// Drain waits until every queued delivery is finished, or ctx ends. Call it
// on shutdown after no new work comes in, while Run is still running.
func (d *DeliverMessages) Drain(ctx context.Context) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for d.Pending() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d deliveries still pending: %w", d.Pending(), ctx.Err())
		case <-t.C:
		}
	}
	return nil
}

// Run starts one worker per endpoint, hands them what the outbox still holds
// from before a restart, and blocks until ctx ends.
func (d *DeliverMessages) Run(ctx context.Context) {
	for ep, q := range d.queues {
		go d.worker(ctx, d.senders[ep], q)
	}
	d.recover(ctx)
	<-ctx.Done()
}

// recover queues deliveries left in the outbox. They get a new audit entry,
// the old one was in memory. Unknown endpoints (config changed) stay in the
// outbox and are logged, so nothing is thrown away silently.
func (d *DeliverMessages) recover(ctx context.Context) {
	left := d.resume
	d.resume = nil
	if len(left) == 0 {
		return
	}
	d.log.Info("resuming deliveries from the spool", "count", len(left))
	byEntry := map[string]*ports.AuditEntry{}
	var order []string
	for i := range left {
		del := &left[i]
		if _, ok := d.queues[del.Endpoint]; !ok {
			d.log.Error("spooled delivery has no endpoint any more, kept in the spool", "id", del.ID, "endpoint", del.Endpoint)
			del.ID = "" // marker: skip
			continue
		}
		del.EntryID = ""
		if !d.auditor.enabled() {
			continue
		}
		key := del.Message.Source + "\x00" + del.Route
		e, ok := byEntry[key]
		if !ok {
			n := d.auditor.newEntry(del.Message.Source, "spool", "", nil, nil)
			n.Reason = "resumed from the spool after a restart"
			e = &n
			byEntry[key] = e
			order = append(order, key)
		}
		one := []domain.Delivery{*del}
		d.auditor.attach(e, one)
		del.EntryID, del.Index = one[0].EntryID, one[0].Index
	}
	for _, k := range order {
		d.audit.Record(*byEntry[k])
	}
	go d.requeue(ctx, left)
}

// requeue hands recovered deliveries to the workers, skipping marked ones.
func (d *DeliverMessages) requeue(ctx context.Context, left []domain.Delivery) {
	for _, del := range left {
		if del.ID == "" {
			continue
		}
		d.pending.Add(1)
		select {
		case d.queues[del.Endpoint] <- job{d: del, done: make(chan error, 1)}:
		case <-ctx.Done():
			d.pending.Add(-1)
			return
		}
	}
}

func (d *DeliverMessages) worker(ctx context.Context, s ports.Sender, q chan job) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-q:
			d.finish(ctx, j, d.attempt(ctx, s, j.d))
		}
	}
}

func (d *DeliverMessages) finish(ctx context.Context, j job, err error) {
	interrupted := err != nil && ctx.Err() != nil
	if interrupted {
		err = fmt.Errorf("%w: %w", ErrInterrupted, err)
	}
	var fallbacks []<-chan error
	if err != nil && !interrupted && !errors.Is(err, ErrExpired) {
		fallbacks = d.startFallbacks(j.d) // before Finish, so the audit entry waits for them
	}
	if d.rec != nil {
		d.rec.RecordDelivery(j.d, err)
	}
	if d.audited(j.d) {
		d.audit.Finish(j.d.EntryID, j.d.Index, d.clock.Now(), err)
	}
	if !interrupted {
		d.forget(j.d.ID)
	}
	d.pending.Add(-1)
	if len(fallbacks) == 0 {
		j.done <- err
		return
	}
	go func() {
		for _, fb := range fallbacks {
			if <-fb == nil {
				j.done <- nil
				return
			}
		}
		j.done <- fmt.Errorf("%w (fallbacks failed too)", err)
	}()
}

// startFallbacks queues the fallback deliveries of a failed delivery.
func (d *DeliverMessages) startFallbacks(failed domain.Delivery) []<-chan error {
	var out []<-chan error
	for _, fb := range failed.Fallback {
		fb.Fallback = nil // one level only
		fb.Created = failed.Created
		fb.EntryID, fb.Index = "", 0
		if d.audited(failed) {
			ad := auditDelivery(fb)
			idx := failed.Index
			ad.FallbackFor = &idx
			if i := d.audit.AddDelivery(failed.EntryID, ad); i >= 0 {
				fb.EntryID, fb.Index = failed.EntryID, i
			}
		}
		d.log.Warn("using fallback", "route", failed.Route, "failed", failed.Destination, "fallback", fb.Destination)
		done, err := d.Enqueue(fb)
		if err != nil {
			d.log.Error("fallback could not be queued", "fallback", fb.Destination, "err", err)
			if d.audited(fb) {
				d.audit.Finish(fb.EntryID, fb.Index, d.clock.Now(), err)
			}
			continue
		}
		out = append(out, done)
	}
	return out
}

func (d *DeliverMessages) forget(id string) {
	if d.outbox == nil || id == "" {
		return
	}
	if err := d.outbox.Done(id); err != nil {
		d.log.Error("spool: could not remove a finished delivery, it may be sent again after a restart", "id", id, "err", err)
	}
}

// attempt applies the age limits and the circuit breaker around deliver.
func (d *DeliverMessages) attempt(ctx context.Context, s ports.Sender, del domain.Delivery) error {
	now := d.clock.Now()
	age := now.Sub(del.Created)
	if d.policy.MaxAge > 0 && age > d.policy.MaxAge {
		d.log.Warn("message too old, not sent", "route", del.Route, "destination", del.Destination, "age", age.Round(time.Second))
		return fmt.Errorf("%w: accepted %s ago, max_age is %s", ErrExpired, roundAge(age), d.policy.MaxAge)
	}
	if d.policy.LateAfter > 0 && age > d.policy.LateAfter {
		del.Message.Markdown += fmt.Sprintf("\n\n⏱ _Late: this was received %s ago._", roundAge(age))
	}
	b := d.breakers[del.Endpoint]
	if b != nil && d.policy.BreakerFailures > 0 {
		b.mu.Lock()
		until := b.openUntil
		b.mu.Unlock()
		if now.Before(until) {
			return fmt.Errorf("%w (until %s)", ErrBreakerOpen, until.Format("15:04:05"))
		}
	}
	err := d.deliver(ctx, s, del)
	if b != nil && d.policy.BreakerFailures > 0 && ctx.Err() == nil && !ports.IsPermanent(err) {
		b.mu.Lock()
		if err == nil {
			b.fails, b.openUntil = 0, time.Time{}
		} else if b.fails++; b.fails >= d.policy.BreakerFailures {
			b.openUntil = d.clock.Now().Add(d.policy.BreakerCooldown)
			d.log.Warn("endpoint keeps failing, skipping it", "endpoint", del.Endpoint, "failures", b.fails, "for", d.policy.BreakerCooldown)
		}
		b.mu.Unlock()
	}
	return err
}

func roundAge(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	case d >= time.Minute:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Millisecond).String()
	}
}

func (d *DeliverMessages) deliver(ctx context.Context, s ports.Sender, del domain.Delivery) error {
	backoff := min(time.Second, d.maxBackoff)
	for attempt := 1; ; attempt++ {
		err := s.Send(ctx, del.Address, del.Message)
		d.recordAttempt(del, err)
		if err == nil {
			d.log.Debug("delivered", "route", del.Route, "destination", del.Destination, "endpoint", del.Endpoint)
			return nil
		}
		if ports.IsPermanent(err) {
			d.log.Error("giving up, the error is permanent", "route", del.Route, "destination", del.Destination, "attempts", attempt, "err", err)
			return err
		}
		if attempt >= d.maxRetries || ctx.Err() != nil {
			d.log.Error("giving up", "route", del.Route, "destination", del.Destination, "attempts", attempt, "err", err)
			return err
		}
		wait := jitter(backoff)
		if ra, ok := ports.RetryAfterOf(err); ok {
			wait = min(ra, maxRetryAfter) // the backend said when, believe it
		}
		d.log.Warn("send failed, retrying", "route", del.Route, "destination", del.Destination, "attempt", attempt, "in", wait, "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(backoff*2, d.maxBackoff)
	}
}

func (d *DeliverMessages) recordAttempt(del domain.Delivery, err error) {
	if !d.audited(del) {
		return
	}
	a := ports.AuditAttempt{Time: d.clock.Now()}
	if err != nil {
		a.Error = err.Error()
	}
	d.audit.Attempt(del.EntryID, del.Index, a)
}

func (d *DeliverMessages) audited(del domain.Delivery) bool {
	return d.audit != nil && del.EntryID != ""
}

// jitter spreads retries by ±20 %, so many failed deliveries do not all
// hit a recovering backend at the same moment.
func jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*mrand.Float64())) //nolint:gosec // jitter, not security
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
