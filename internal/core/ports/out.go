package ports

import (
	"context"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
)

// Sender delivers messages for one endpoint (an account of a target).
// It returns an error if the message did not arrive, the caller retries.
type Sender interface {
	Send(ctx context.Context, address string, msg domain.Message) error
}

// Outbox keeps deliveries on durable storage from the moment they are
// accepted until they are finished, so a restart or crash does not lose
// them. Without an outbox, queued deliveries live in memory only.
//
// It is the seam for a replicated log later (NATS JetStream): such an
// adapter implements the same three calls.
type Outbox interface {
	// Put stores d under d.ID, replacing an earlier version. It returns
	// only once d is durable.
	Put(d domain.Delivery) error
	// Done removes a finished delivery. Unknown IDs are not an error.
	Done(id string) error
	// Pending returns everything not done yet, oldest first.
	Pending() ([]domain.Delivery, error)
}

// Coordinator holds state that may be shared between nodes: heartbeat times,
// dedupe keys and who leads. A single node uses a local implementation.
type Coordinator interface {
	NodeID() string
	IsLeader() bool
	// SetHeartbeat records a heartbeat, an older time never replaces a newer one.
	SetHeartbeat(name string, at time.Time)
	Heartbeat(name string) (time.Time, bool)
	// Seen reports whether key was claimed and has not expired.
	Seen(key string) bool
	// Claim marks key as seen for ttl and reports whether it was new.
	Claim(key string, ttl time.Duration) bool
	Status() ClusterStatus
	// Run keeps the coordinator going until ctx ends.
	Run(ctx context.Context)
}

type ClusterStatus struct {
	Enabled bool         `json:"enabled"`
	NodeID  string       `json:"nodeId"`
	Leader  string       `json:"leader"`
	Peers   []PeerStatus `json:"peers,omitempty"`
}

type PeerStatus struct {
	ID       string    `json:"id"`
	URL      string    `json:"url,omitempty"`
	Self     bool      `json:"self"`
	Alive    bool      `json:"alive"`
	LastSeen time.Time `json:"lastSeen"`
	Error    string    `json:"error,omitempty"`
}

// Recorder receives what happened, for counters and the UI.
type Recorder interface {
	RecordEvent(EventRecord)
	RecordDelivery(d domain.Delivery, err error)
}

// StatsReader returns what a Recorder collected, newest events first.
type StatsReader interface {
	Snapshot() StatsSnapshot
}

// Clock tells the time. Use cases never call time.Now directly, so tests
// can move time forward.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

type EventRecord struct {
	Time    time.Time `json:"time"`
	Node    string    `json:"node,omitempty"`
	Source  string    `json:"source"`
	Status  string    `json:"status,omitempty"`
	Routes  []string  `json:"routes,omitempty"`
	Dropped string    `json:"dropped,omitempty"`
	Verdict string    `json:"verdict"` // delivered, queued, failed, dropped, unmatched, duplicate, rejected, error, heartbeat
}

type StatsSnapshot struct {
	Sources      map[string]SourceStats `json:"sources"`
	Routes       map[string]RouteStats  `json:"routes"`
	Destinations map[string]DestStats   `json:"destinations"`
	Events       []EventRecord          `json:"events"`
}

type SourceStats struct {
	Received int       `json:"received"`
	LastSeen time.Time `json:"lastSeen"`
}

type RouteStats struct {
	Matched   int       `json:"matched"`
	Delivered int       `json:"delivered"`
	Failed    int       `json:"failed"`
	LastMatch time.Time `json:"lastMatch"`
	LastError string    `json:"lastError,omitempty"`
}

type DestStats struct {
	Delivered int       `json:"delivered"`
	Failed    int       `json:"failed"`
	LastSent  time.Time `json:"lastSent"`
}

// SilenceStore keeps silences. Expired ones may be kept or dropped, the
// application filters by time.
type SilenceStore interface {
	Save(domain.Silence) error
	Delete(id string) error
	All() ([]domain.Silence, error)
}

// Receiver reads messages back from a target, to prove that they arrive
// and can be read (for Matrix: decrypted by another account).
type Receiver interface {
	// Expect starts watching address for a message that contains marker.
	// Call it before sending. wait blocks until the message arrived or
	// ctx ends.
	Expect(address, marker string) (wait func(ctx context.Context) error)
}
