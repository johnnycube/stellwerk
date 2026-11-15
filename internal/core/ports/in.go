// Package ports defines the boundary of the core.
//
// Driving ports (in.go) are the use cases: the only way into the core.
// Driven ports (out.go) are what the use cases need from the outside world.
//
// Adapters in internal/adapters implement or call these. The core never
// imports an adapter, internal/archtest enforces that. Every driven port has a
// contract suite in ports/portstest that each implementation must pass.
package ports

import (
	"context"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
)

// IngestEvents takes decoded events from a sender and gets them where they
// belong: heartbeats are recorded, duplicates skipped, the rest routed and
// delivered.
type IngestEvents interface {
	Ingest(ctx context.Context, req IngestRequest) IngestResult
	// Refuse records a request that was received but turned away before
	// routing (bad payload, rate limit), so it shows up in the audit log.
	Refuse(req RefusedRequest)
}

type IngestRequest struct {
	Events []*domain.Event
	Raw    []byte // original payload, used for dedupe keys and the audit log
}

type RefusedRequest struct {
	Source string
	Kind   string
	Reason string
	Raw    []byte
}

type Outcome int

const (
	NothingToDo Outcome = iota // heartbeats, duplicates, drops, no match
	Delivered                  // sync mode: all deliveries succeeded
	Accepted                   // async mode: queued
	Failed                     // a delivery failed, the sender should retry
	TimedOut                   // sync mode: deliveries still running, the sender should retry
	Busy                       // queue full, the sender should retry later
	Broken                     // a bad rule or template, retrying will not help
)

func (o Outcome) String() string {
	return [...]string{"nothing to do", "delivered", "accepted", "failed", "timed out", "busy", "broken"}[o]
}

type IngestResult struct {
	Outcome Outcome
	Detail  string
}

// SimulateEvents shows what would happen to events, without recording,
// deduplicating or delivering anything.
type SimulateEvents interface {
	Simulate(events []*domain.Event) []Simulation
}

type Simulation struct {
	Event      *domain.Event
	Heartbeat  string // name if the event is a heartbeat, then nothing else applies
	Trace      []domain.TraceStep
	Deliveries []domain.Delivery
	DroppedBy  string
	Silenced   string // why the deliveries would be muted, if a silence matches
	Err        string
}

// DescribeTopology returns the whole setup and its live state, for the UI.
type DescribeTopology interface {
	Describe() Topology
}

type Topology struct {
	Engine       string // default expression engine
	Silences     []domain.Silence
	Rules        []domain.Rule
	Destinations []domain.Destination
	Heartbeats   []HeartbeatState
	Templates    []Template
	Cluster      ClusterStatus
	Stats        StatsSnapshot
}

type Template struct {
	Name    string
	Source  string
	Builtin bool
}

type HeartbeatState struct {
	domain.Heartbeat
	LastSeen time.Time
	Missing  bool
	Failed   bool // the job reported a failure (ping .../fail) and has not succeeded since
}

// ReceivePings is the healthchecks-style side of heartbeats: a cron job
// calls a URL when it ran, or another one when it failed.
type ReceivePings interface {
	// Ping records that the job ran. After a failure it sends a recovery.
	Ping(name string) error
	// Fail alerts at once that the job failed, with an optional detail.
	Fail(name, detail string) error
}

// CheckHeartbeats looks at every heartbeat and alerts when one stopped or
// came back. A scheduler calls it periodically.
type CheckHeartbeats interface {
	Check() []HeartbeatChange
	States() []HeartbeatState
}

// HeartbeatChange is what one check changed. Sent is false on a node that is
// not the leader: it tracks the state but leaves the alert to the leader.
type HeartbeatChange struct {
	Name    string
	Missing bool // true: went missing, false: restored
	Sent    bool
}

// ReadAudit looks at the audit log: every received message, the rules'
// verdict and every send attempt.
type ReadAudit interface {
	List(q AuditQuery) []AuditEntry
	Get(id string) (AuditEntry, bool)
}

// ManageSilences mutes events for a while: from the UI, the API or a chat
// command.
type ManageSilences interface {
	// Silence mutes what matches for the given time.
	Silence(SilenceRequest) (domain.Silence, error)
	// Expire ends a silence now. Unknown IDs are an error.
	Expire(id string) error
	// Active lists the silences in force, the ones ending first come first.
	Active() []domain.Silence
}

type SilenceRequest struct {
	// ID is optional. Given the same ID, every node of a cluster creates
	// the same silence (a chat command reaches all of them), and a repeat
	// returns the existing one.
	ID       string
	Matchers string // e.g. "source=k8s severity=warning", see domain.ParseMatchers
	For      string // e.g. "2h", "1d"
	By       string // who, e.g. "@me:example.org" or "ui"
	Comment  string
}

// ResendAudit sends the messages of an audit entry again, e.g. after a
// target was fixed.
type ResendAudit interface {
	// Resend sends the entry's messages again (only the failed ones if
	// onlyFailed) and returns the ID of the new audit entry.
	Resend(id string, onlyFailed bool) (string, error)
}

// RunCanary sends a test message through the whole chain and reports
// whether it arrived.
type RunCanary interface {
	Run(ctx context.Context) CanaryResult
	Last() (CanaryResult, bool)
}

type CanaryResult struct {
	Time     time.Time `json:"time"`
	OK       bool      `json:"ok"`
	Marker   string    `json:"marker"`
	Problems []string  `json:"problems,omitempty"`
}
