// Package portstest is the test kit for the ports: in-memory fakes for every
// driven port, and contract suites that any implementation must pass.
//
// The fakes are checked against the same contracts as the real adapters, so
// a use case test with fakes means the same as one with real adapters.
package portstest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// ---- Clock ----

// Clock is a manual clock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock() *Clock { return &Clock{now: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// ---- Sender ----

type Sent struct {
	Address string
	Msg     domain.Message
}

// Sender records messages. It can fail a number of times, fail always, or
// block until released.
type Sender struct {
	mu        sync.Mutex
	sent      []Sent
	attempts  int
	failNext  int
	failAll   bool
	Err       error
	block     chan struct{}
	attempted chan struct{}
}

var ErrDown = errors.New("fake sender down")

func NewSender() *Sender { return &Sender{Err: ErrDown, attempted: make(chan struct{}, 1000)} }

func (s *Sender) FailNext(n int)  { s.mu.Lock(); s.failNext = n; s.mu.Unlock() }
func (s *Sender) FailAll(on bool) { s.mu.Lock(); s.failAll = on; s.mu.Unlock() }
func (s *Sender) Block()          { s.mu.Lock(); s.block = make(chan struct{}); s.mu.Unlock() }

func (s *Sender) Release()                   { s.mu.Lock(); close(s.block); s.block = nil; s.mu.Unlock() }
func (s *Sender) Attempted() <-chan struct{} { return s.attempted }

func (s *Sender) Send(ctx context.Context, address string, msg domain.Message) error {
	s.mu.Lock()
	s.attempts++
	block := s.block
	s.mu.Unlock()
	s.attempted <- struct{}{}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAll {
		return s.Err
	}
	if s.failNext > 0 {
		s.failNext--
		return s.Err
	}
	s.sent = append(s.sent, Sent{address, msg})
	return nil
}

func (s *Sender) Sent() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sent(nil), s.sent...)
}

func (s *Sender) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// ---- Coordinator ----

// Coordinator is an in-memory coordinator with a switchable leader flag.
type Coordinator struct {
	mu         sync.Mutex
	id         string
	leader     bool
	clock      ports.Clock
	heartbeats map[string]time.Time
	seen       map[string]time.Time
}

func NewCoordinator(id string, clock ports.Clock) *Coordinator {
	return &Coordinator{id: id, leader: true, clock: clock, heartbeats: map[string]time.Time{}, seen: map[string]time.Time{}}
}

func (c *Coordinator) SetLeader(on bool) { c.mu.Lock(); c.leader = on; c.mu.Unlock() }
func (c *Coordinator) NodeID() string    { return c.id }
func (c *Coordinator) IsLeader() bool    { c.mu.Lock(); defer c.mu.Unlock(); return c.leader }

func (c *Coordinator) SetHeartbeat(name string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.After(c.heartbeats[name]) {
		c.heartbeats[name] = at
	}
}

func (c *Coordinator) Heartbeat(name string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.heartbeats[name]
	return t, ok
}

func (c *Coordinator) Seen(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.seen[key]
	return ok && exp.After(c.clock.Now())
}

func (c *Coordinator) Claim(key string, ttl time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	if exp, ok := c.seen[key]; ok && exp.After(now) {
		return false
	}
	c.seen[key] = now.Add(ttl)
	return true
}

func (c *Coordinator) Status() ports.ClusterStatus {
	leader := ""
	if c.IsLeader() {
		leader = c.id
	}
	return ports.ClusterStatus{NodeID: c.id, Leader: leader}
}

func (c *Coordinator) Run(ctx context.Context) { <-ctx.Done() }

// ---- Recorder ----

// Recorder keeps everything it is told, and implements StatsReader like the
// real one so contract tests can run against it.
type Recorder struct {
	mu         sync.Mutex
	Events     []ports.EventRecord
	Deliveries []RecordedDelivery
}

type RecordedDelivery struct {
	Delivery domain.Delivery
	Err      error
}

func NewRecorder() *Recorder { return &Recorder{} }

func (r *Recorder) RecordEvent(e ports.EventRecord) {
	r.mu.Lock()
	r.Events = append(r.Events, e)
	r.mu.Unlock()
}

func (r *Recorder) RecordDelivery(d domain.Delivery, err error) {
	r.mu.Lock()
	r.Deliveries = append(r.Deliveries, RecordedDelivery{d, err})
	r.mu.Unlock()
}

// Verdicts returns the recorded event verdicts in order.
func (r *Recorder) Verdicts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.Events))
	for i, e := range r.Events {
		out[i] = e.Verdict
	}
	return out
}

func (r *Recorder) DeliveryCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Deliveries)
}

func (r *Recorder) Snapshot() ports.StatsSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := ports.StatsSnapshot{Sources: map[string]ports.SourceStats{}, Routes: map[string]ports.RouteStats{}, Destinations: map[string]ports.DestStats{}}
	for _, e := range r.Events {
		src := s.Sources[e.Source]
		src.Received++
		src.LastSeen = e.Time
		s.Sources[e.Source] = src
		for _, name := range e.Routes {
			rs := s.Routes[name]
			rs.Matched++
			rs.LastMatch = e.Time
			s.Routes[name] = rs
		}
	}
	for _, d := range r.Deliveries {
		rs, ds := s.Routes[d.Delivery.Route], s.Destinations[d.Delivery.Destination]
		if d.Err != nil {
			rs.Failed++
			rs.LastError = d.Err.Error()
			ds.Failed++
		} else {
			rs.Delivered++
			ds.Delivered++
		}
		s.Routes[d.Delivery.Route], s.Destinations[d.Delivery.Destination] = rs, ds
	}
	for i := len(r.Events) - 1; i >= 0; i-- {
		s.Events = append(s.Events, r.Events[i])
	}
	return s
}

// SortedKeys is a small helper for assertions on maps.
func SortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	_ ports.Clock       = (*Clock)(nil)
	_ ports.Sender      = (*Sender)(nil)
	_ ports.Coordinator = (*Coordinator)(nil)
	_ ports.Recorder    = (*Recorder)(nil)
	_ ports.StatsReader = (*Recorder)(nil)
)

// ---- AuditStore ----

// AuditStore keeps every entry in memory, unbounded.
type AuditStore struct {
	mu      sync.Mutex
	entries []*ports.AuditEntry
	byID    map[string]*ports.AuditEntry
}

var _ ports.AuditStore = (*AuditStore)(nil)

func NewAuditStore() *AuditStore { return &AuditStore{byID: map[string]*ports.AuditEntry{}} }

func (s *AuditStore) Record(e ports.AuditEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := e.Clone()
	s.entries = append(s.entries, &c)
	s.byID[c.ID] = &c
}

func (s *AuditStore) Attempt(id string, i int, a ports.AuditAttempt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		e.AddAttempt(i, a)
	}
}

func (s *AuditStore) Finish(id string, i int, at time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		e.FinishDelivery(i, at, err)
	}
}

func (s *AuditStore) AddDelivery(id string, d ports.AuditDelivery) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byID[id]; ok {
		return e.AddDelivery(d)
	}
	return -1
}

func (s *AuditStore) List(q ports.AuditQuery) []ports.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.AuditEntry
	for i := len(s.entries) - 1; i >= 0 && (q.Limit <= 0 || len(out) < q.Limit); i-- {
		if s.entries[i].Matches(q) {
			out = append(out, s.entries[i].Clone())
		}
	}
	return out
}

func (s *AuditStore) Get(id string) (ports.AuditEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[id]
	if !ok {
		return ports.AuditEntry{}, false
	}
	return e.Clone(), true
}

// All returns every entry, oldest first.
func (s *AuditStore) All() []ports.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ports.AuditEntry, len(s.entries))
	for i, e := range s.entries {
		out[i] = e.Clone()
	}
	return out
}

// ---- Outbox ----

// Outbox keeps deliveries in memory, in the order they were first put.
// FailPut makes Put fail, to test a broken disk.
type Outbox struct {
	mu      sync.Mutex
	order   []string
	byID    map[string]domain.Delivery
	failPut bool
}

var _ ports.Outbox = (*Outbox)(nil)

var ErrDiskFull = errors.New("disk full")

func NewOutbox() *Outbox { return &Outbox{byID: map[string]domain.Delivery{}} }

func (o *Outbox) FailPut(on bool) { o.mu.Lock(); o.failPut = on; o.mu.Unlock() }

func (o *Outbox) Put(d domain.Delivery) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failPut {
		return ErrDiskFull
	}
	if _, ok := o.byID[d.ID]; !ok {
		o.order = append(o.order, d.ID)
	}
	o.byID[d.ID] = d
	return nil
}

func (o *Outbox) Done(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.byID[id]; !ok {
		return nil
	}
	delete(o.byID, id)
	for i, x := range o.order {
		if x == id {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
	return nil
}

func (o *Outbox) Pending() ([]domain.Delivery, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]domain.Delivery, 0, len(o.order))
	for _, id := range o.order {
		out = append(out, o.byID[id])
	}
	return out, nil
}

// Len is the number of deliveries not done.
func (o *Outbox) Len() int { o.mu.Lock(); defer o.mu.Unlock(); return len(o.order) }

// ---- SilenceStore ----

type SilenceStore struct {
	mu   sync.Mutex
	byID map[string]domain.Silence
	Fail bool
}

var _ ports.SilenceStore = (*SilenceStore)(nil)

func NewSilenceStore() *SilenceStore { return &SilenceStore{byID: map[string]domain.Silence{}} }

func (s *SilenceStore) Save(x domain.Silence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Fail {
		return ErrDiskFull
	}
	s.byID[x.ID] = x
	return nil
}

func (s *SilenceStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
	return nil
}

func (s *SilenceStore) All() ([]domain.Silence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Silence, 0, len(s.byID))
	for _, k := range SortedKeys(s.byID) {
		out = append(out, s.byID[k])
	}
	return out, nil
}
