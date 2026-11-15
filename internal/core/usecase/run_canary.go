package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// CanarySpec says what the canary tests.
type CanarySpec struct {
	To      []string      // destinations
	Alert   []string      // where a failed check is reported, empty: the fallbacks of To
	Timeout time.Duration // how long delivery and read-back may take
}

// RunCanary implements ports.RunCanary. The test message goes through the
// real queues, retries and fallbacks. If it cannot be delivered, the
// fallbacks get it, marked, and that is the alarm. With a Receiver, another
// account must also read it, which proves the room still works end to end
// (for Matrix: the message can be decrypted).
type RunCanary struct {
	spec     CanarySpec
	router   atomic.Pointer[domain.Router]
	out      Enqueuer
	receiver ports.Receiver
	clock    ports.Clock
	log      *slog.Logger
	audit    *auditor

	mu   sync.Mutex
	last *ports.CanaryResult
}

var _ ports.RunCanary = (*RunCanary)(nil)

func NewRunCanary(spec CanarySpec, router *domain.Router, out Enqueuer, receiver ports.Receiver, clock ports.Clock, log *slog.Logger) *RunCanary {
	if spec.Timeout <= 0 {
		spec.Timeout = 2 * time.Minute
	}
	u := &RunCanary{spec: spec, out: out, receiver: receiver, clock: clock, log: log}
	u.router.Store(router)
	return u
}

// SetRouter switches to a reloaded configuration.
func (u *RunCanary) SetRouter(r *domain.Router) { u.router.Store(r) }

// WithAudit records every canary run.
func (u *RunCanary) WithAudit(store ports.AuditStore, node string) *RunCanary {
	u.audit = newAuditor(store, u.clock, node, AuditPolicy{})
	return u
}

func (u *RunCanary) Last() (ports.CanaryResult, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil {
		return ports.CanaryResult{}, false
	}
	return *u.last, true
}

func (u *RunCanary) Run(ctx context.Context) ports.CanaryResult {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	res := ports.CanaryResult{Time: u.clock.Now(), Marker: "canary-" + hex.EncodeToString(b), OK: true}
	msg := domain.Message{
		Markdown: fmt.Sprintf("🐤 Canary `%s`: stellwerk checks that messages get here.", res.Marker),
		Priority: domain.PriorityLow, Source: "stellwerk", Route: "canary",
	}

	ds := make([]domain.Delivery, 0, len(u.spec.To))
	for _, alias := range u.spec.To {
		ds = append(ds, u.router.Load().Delivery("canary", alias, msg))
	}
	waits := make([]func(context.Context) error, len(ds))
	if u.receiver != nil {
		for i, d := range ds {
			waits[i] = u.receiver.Expect(d.Address, res.Marker)
		}
	}
	var entry ports.AuditEntry
	if u.audit.enabled() {
		entry = u.audit.newEntry("stellwerk", "canary", "", map[string]string{"marker": res.Marker}, nil)
		u.audit.attach(&entry, ds)
		u.audit.store.Record(entry)
	}
	ctx, cancel := context.WithTimeout(ctx, u.spec.Timeout)
	defer cancel()
	for i, d := range ds {
		if problem := u.check(ctx, entry.ID, i, d, waits[i]); problem != "" {
			res.OK = false
			res.Problems = append(res.Problems, problem)
		}
	}
	if res.OK {
		u.log.Info("canary arrived", "marker", res.Marker, "destinations", len(ds))
	} else {
		u.log.Error("canary failed", "marker", res.Marker, "problems", strings.Join(res.Problems, "; "))
	}
	u.mu.Lock()
	u.last = &res
	u.mu.Unlock()
	return res
}

// check sends the canary to delivery i and returns the problem, or "".
func (u *RunCanary) check(ctx context.Context, entryID string, i int, d domain.Delivery, wait func(context.Context) error) string {
	if err := u.send(ctx, entryID, i, d); err != nil {
		u.failed(d, err)
		return fmt.Sprintf("%s: %v", d.Destination, err)
	}
	if wait == nil {
		return ""
	}
	if err := wait(ctx); err != nil {
		u.alarm(d, "was accepted by the server but did not arrive readable. Members may not be able to decrypt it, or the bot lost access.")
		return fmt.Sprintf("%s: delivered, but not received back within %s", d.Destination, u.spec.Timeout)
	}
	return ""
}

// send delivers without fallbacks: arriving only through one fails the canary.
func (u *RunCanary) send(ctx context.Context, entryID string, i int, d domain.Delivery) error {
	d.Fallback = nil
	done, err := u.out.Enqueue(d)
	if err != nil {
		if u.audit.enabled() {
			u.audit.store.Finish(entryID, i, u.clock.Now(), err)
		}
		return err
	}
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("not delivered within %s", u.spec.Timeout)
	}
}

// failed raises the alarm for a canary that could not be delivered.
func (u *RunCanary) failed(d domain.Delivery, err error) {
	if len(d.Fallback) == 0 || len(u.spec.Alert) > 0 {
		u.alarm(d, fmt.Sprintf("could not be delivered: %v", err))
		return
	}
	for _, fb := range d.Fallback { // the canary with "could not deliver to ..." is the alarm
		fb.Message.Loud, fb.Message.Priority = true, domain.PriorityHigh
		if _, err := u.out.Enqueue(fb); err != nil {
			u.log.Error("canary fallback could not be queued", "to", fb.Destination, "err", err)
		}
	}
}

// alarm reports a failed check to canary.alert, or to the fallbacks of the
// destination that failed.
func (u *RunCanary) alarm(d domain.Delivery, what string) {
	msg := domain.Message{
		Markdown: fmt.Sprintf("🚨 **Canary to `%s` failed**: the test message %s", d.Destination, what),
		Loud:     true, Priority: domain.PriorityHigh, Source: "stellwerk", Route: "canary",
	}
	var targets []domain.Delivery
	for _, alias := range u.spec.Alert {
		targets = append(targets, u.router.Load().Delivery("canary", alias, msg))
	}
	if len(u.spec.Alert) == 0 {
		for _, fb := range d.Fallback {
			targets = append(targets, u.router.Load().Delivery("canary", fb.Destination, msg))
		}
	}
	if len(targets) == 0 {
		u.log.Error("canary failed and there is nowhere to report it: set canary.alert or a fallback", "destination", d.Destination)
	}
	for _, t := range targets {
		t.Fallback = nil
		if _, err := u.out.Enqueue(t); err != nil {
			u.log.Error("canary alarm could not be queued", "to", t.Destination, "err", err)
		}
	}
}
