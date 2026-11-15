package usecase

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// CheckHeartbeats implements ports.CheckHeartbeats.
//
// Every node evaluates every heartbeat and remembers whether it is missing.
// Only the leader sends the alert and the recovery. So when leadership moves,
// the new leader already knows an alert is out and does not send it twice.
// A heartbeat never seen gets a grace period from the use case's start.
type CheckHeartbeats struct {
	specs   []domain.Heartbeat
	coord   ports.Coordinator
	router  *domain.Router
	out     Enqueuer
	clock   ports.Clock
	log     *slog.Logger
	started time.Time

	mu       sync.Mutex
	missing  map[string]bool
	failed   map[string]bool
	audit    *auditor
	silences Silencer
}

// WithSilences mutes heartbeat alarms that match a silence: the event
// {source: <heartbeat source>, labels: {heartbeat: <name>}}.
func (u *CheckHeartbeats) WithSilences(s Silencer) *CheckHeartbeats {
	u.silences = s
	return u
}

// SetRouting switches to reloaded heartbeats and destinations. Missing
// state is kept for heartbeats that still exist.
func (u *CheckHeartbeats) SetRouting(specs []domain.Heartbeat, router *domain.Router) {
	u.mu.Lock()
	defer u.mu.Unlock()
	keep := map[string]bool{}
	failed := map[string]bool{}
	for _, s := range specs {
		keep[s.Name] = u.missing[s.Name]
		failed[s.Name] = u.failed[s.Name]
	}
	u.specs, u.router, u.missing, u.failed = specs, router, keep, failed
}

// WithAudit gives every heartbeat alert an audit entry with its deliveries.
func (u *CheckHeartbeats) WithAudit(store ports.AuditStore, p AuditPolicy) *CheckHeartbeats {
	u.audit = newAuditor(store, u.clock, u.coord.NodeID(), p)
	return u
}

var (
	_ ports.CheckHeartbeats = (*CheckHeartbeats)(nil)
	_ ports.ReceivePings    = (*CheckHeartbeats)(nil)
)

func NewCheckHeartbeats(specs []domain.Heartbeat, coord ports.Coordinator, router *domain.Router, out Enqueuer, clock ports.Clock, log *slog.Logger) *CheckHeartbeats {
	return &CheckHeartbeats{specs: specs, coord: coord, router: router, out: out, clock: clock, log: log, started: clock.Now(), missing: map[string]bool{}, failed: map[string]bool{}}
}

func (u *CheckHeartbeats) Check() []ports.HeartbeatChange {
	u.mu.Lock()
	defer u.mu.Unlock()
	leader := u.coord.IsLeader()
	now := u.clock.Now()
	var changes []ports.HeartbeatChange
	for _, s := range u.specs {
		silent := now.Sub(u.lastSeen(s.Name))
		switch {
		case silent > s.Timeout && !u.missing[s.Name]:
			u.missing[s.Name] = true
			if leader {
				u.send(s, "missing", missingText(s, silent), true)
			}
			changes = append(changes, ports.HeartbeatChange{Name: s.Name, Missing: true, Sent: leader})
		case silent <= s.Timeout && u.missing[s.Name]:
			u.missing[s.Name] = false
			if leader {
				u.send(s, "restored", restoredText(s), false)
			}
			changes = append(changes, ports.HeartbeatChange{Name: s.Name, Missing: false, Sent: leader})
		}
	}
	return changes
}

// lastSeen is the last heartbeat, but not before the use case started.
func (u *CheckHeartbeats) lastSeen(name string) time.Time {
	last, ok := u.coord.Heartbeat(name)
	if !ok || last.Before(u.started) {
		last = maxTime(last, u.started)
	}
	return last
}

func missingText(s domain.Heartbeat, silent time.Duration) string {
	if s.Source == "" {
		return fmt.Sprintf("🚨 **%s did not check in** for %s.", s.Name, silent.Round(time.Second))
	}
	return fmt.Sprintf("🚨 **Heartbeat missing**: no `%s` from `%s` for %s. Alerts from there are not reaching you.", s.Name, s.Source, silent.Round(time.Second))
}

func restoredText(s domain.Heartbeat) string {
	if s.Source == "" {
		return fmt.Sprintf("✅ **%s checked in** again.", s.Name)
	}
	return fmt.Sprintf("✅ **Heartbeat restored**: `%s` from `%s` is arriving again.", s.Name, s.Source)
}

func (u *CheckHeartbeats) send(s domain.Heartbeat, status, text string, loud bool) {
	msg := domain.Message{Markdown: text, Loud: loud, Priority: domain.PriorityNormal, Source: s.Source, Route: "heartbeat:" + s.Name}
	if loud {
		msg.Priority = domain.PriorityUrgent // the alarm about missing alarms
		msg.Mentions = s.Mention
	}
	ds := make([]domain.Delivery, 0, len(s.To))
	for _, alias := range s.To {
		ds = append(ds, u.router.Delivery(msg.Route, alias, msg))
	}
	if u.silenced(s, status) {
		return
	}
	var entry ports.AuditEntry
	if u.audit.enabled() {
		entry = u.audit.newEntry("stellwerk", "heartbeat-alert", status, map[string]string{"heartbeat": s.Name, "source": s.Source}, nil)
		entry.Heartbeat = s.Name
		u.audit.attach(&entry, ds)
		u.audit.store.Record(entry)
	}
	for i, d := range ds {
		if _, err := u.out.Enqueue(d); err != nil {
			u.log.Error("heartbeat alert could not be queued", "heartbeat", s.Name, "err", err)
			if u.audit.enabled() {
				u.audit.store.Finish(entry.ID, i, u.clock.Now(), err)
			}
		}
	}
}

// silenced reports whether a silence mutes the alarm, and records that it did.
func (u *CheckHeartbeats) silenced(s domain.Heartbeat, status string) bool {
	if u.silences == nil {
		return false
	}
	sil, ok := u.silences.Find(&domain.Event{Source: s.Source, Kind: "heartbeat", Status: status, Labels: map[string]string{"heartbeat": s.Name}})
	if !ok {
		return false
	}
	u.log.Info("heartbeat alarm silenced", "heartbeat", s.Name, "silence", sil.ID)
	if u.audit.enabled() {
		e := u.audit.newEntry("stellwerk", "heartbeat-alert", status, map[string]string{"heartbeat": s.Name, "source": s.Source}, nil)
		e.Heartbeat, e.Verdict, e.SilencedBy, e.Reason = s.Name, ports.AuditSilenced, sil.ID, silencedReason(sil)
		u.audit.store.Record(e)
	}
	return true
}

// Ping records a run of the named job. Pings are not tied to the leader:
// the node that gets the request answers it, as only one node gets it.
func (u *CheckHeartbeats) Ping(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.spec(name)
	if !ok {
		return fmt.Errorf("heartbeat %q: %w", name, ErrNotFound)
	}
	u.coord.SetHeartbeat(name, u.clock.Now())
	if u.failed[name] {
		u.failed[name] = false
		u.send(s, "succeeded", fmt.Sprintf("✅ **%s succeeded** again.", name), false)
	}
	return nil
}

// Fail alerts that the named job failed. It counts as a sign of life: the
// job ran, so the missing alarm does not fire on top.
func (u *CheckHeartbeats) Fail(name, detail string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.spec(name)
	if !ok {
		return fmt.Errorf("heartbeat %q: %w", name, ErrNotFound)
	}
	u.coord.SetHeartbeat(name, u.clock.Now())
	u.failed[name] = true
	text := fmt.Sprintf("❌ **%s failed**", name)
	if detail = strings.TrimSpace(detail); detail != "" {
		text += "\n\n```\n" + domain.Truncate(detail, 1500) + "\n```"
	}
	u.send(s, "failed", text, true)
	return nil
}

func (u *CheckHeartbeats) spec(name string) (domain.Heartbeat, bool) {
	for _, s := range u.specs {
		if s.Name == name {
			return s, true
		}
	}
	return domain.Heartbeat{}, false
}

func (u *CheckHeartbeats) States() []ports.HeartbeatState {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]ports.HeartbeatState, 0, len(u.specs))
	for _, s := range u.specs {
		last, _ := u.coord.Heartbeat(s.Name)
		out = append(out, ports.HeartbeatState{Heartbeat: s, LastSeen: last, Missing: u.missing[s.Name], Failed: u.failed[s.Name]})
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
