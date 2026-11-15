package ports

import (
	"strings"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
)

// AuditEntry is the full story of one received message: what came in, how
// the rules decided, and every attempt to send it, including errors.
// Heartbeat alerts that stellwerk sends itself get an entry too.
type AuditEntry struct {
	ID               string             `json:"id"`
	Time             time.Time          `json:"time"`
	Node             string             `json:"node,omitempty"`
	Source           string             `json:"source"`
	Kind             string             `json:"kind,omitempty"`
	Status           string             `json:"status,omitempty"`
	Labels           map[string]string  `json:"labels,omitempty"`
	Payload          string             `json:"payload,omitempty"`
	PayloadTruncated bool               `json:"payloadTruncated,omitempty"`
	Verdict          string             `json:"verdict"` // see Audit* verdict constants
	Reason           string             `json:"reason,omitempty"`
	Trace            []domain.TraceStep `json:"trace,omitempty"`
	DroppedBy        string             `json:"droppedBy,omitempty"`
	Heartbeat        string             `json:"heartbeat,omitempty"`
	SilencedBy       string             `json:"silencedBy,omitempty"` // silence ID
	ResentFrom       string             `json:"resentFrom,omitempty"` // entry ID this one repeats
	Deliveries       []AuditDelivery    `json:"deliveries,omitempty"`
}

// Verdicts of an audit entry. Pending turns into delivered or failed when the
// last delivery finishes.
const (
	AuditPending   = "pending"
	AuditDelivered = "delivered"
	AuditFallback  = "fallback" // a destination failed, its fallback delivered
	AuditFailed    = "failed"
	AuditDropped   = "dropped"
	AuditUnmatched = "unmatched"
	AuditDuplicate = "duplicate"
	AuditHeartbeat = "heartbeat"
	AuditRejected  = "rejected" // refused before routing: bad payload, rate limit, full queue
	AuditSilenced  = "silenced" // matched a silence, not sent
	AuditError     = "error"    // a broken rule or template
)

type AuditDelivery struct {
	Route       string         `json:"route"`
	Destination string         `json:"destination"`
	Endpoint    string         `json:"endpoint"`
	Address     string         `json:"address,omitempty"`
	Loud        bool           `json:"loud"`
	Priority    string         `json:"priority,omitempty"`
	Digest      string         `json:"digest,omitempty"` // collected for a digest of this window
	Message     string         `json:"message"`          // the rendered Markdown that was sent
	Status      string         `json:"status"`           // pending, sent, failed
	Attempts    []AuditAttempt `json:"attempts,omitempty"`
	FinishedAt  time.Time      `json:"finishedAt,omitempty"`
	// FallbackFor is the index of the delivery this one stands in for.
	FallbackFor *int `json:"fallbackFor,omitempty"`
}

type AuditAttempt struct {
	Time  time.Time `json:"time"`
	Error string    `json:"error,omitempty"` // empty: this attempt succeeded
}

// Settled reports whether nothing about the entry will change any more.
func (e AuditEntry) Settled() bool { return e.Verdict != AuditPending }

type AuditQuery struct {
	Source  string // exact source name
	Verdict string // exact verdict
	Text    string // case-insensitive search in payload, labels, reasons and errors
	Limit   int    // 0 = default
}

// AuditStore keeps audit entries. Deliveries are addressed by entry ID and
// their index in Deliveries.
type AuditStore interface {
	Record(AuditEntry)
	Attempt(id string, delivery int, a AuditAttempt)
	Finish(id string, delivery int, at time.Time, err error)
	// AddDelivery appends a delivery to a pending entry (a fallback) and
	// returns its index, or -1 if the entry is unknown or settled.
	AddDelivery(id string, d AuditDelivery) int
	List(q AuditQuery) []AuditEntry // newest first
	Get(id string) (AuditEntry, bool)
}

// The methods below are the shared rules for every AuditStore, so stores
// only hold and index entries.

// AddAttempt appends an attempt to delivery i. Out-of-range i is ignored.
func (e *AuditEntry) AddAttempt(i int, a AuditAttempt) {
	if i >= 0 && i < len(e.Deliveries) {
		e.Deliveries[i].Attempts = append(e.Deliveries[i].Attempts, a)
	}
}

// AddDelivery appends d to a pending entry and returns its index, or -1.
func (e *AuditEntry) AddDelivery(d AuditDelivery) int {
	if e.Verdict != AuditPending {
		return -1
	}
	if d.Status == "" {
		d.Status = "pending"
	}
	e.Deliveries = append(e.Deliveries, d)
	return len(e.Deliveries) - 1
}

// FinishDelivery marks delivery i as sent or failed. When the last pending
// delivery finishes, a pending entry becomes delivered or failed, and true
// is returned: the entry just settled.
func (e *AuditEntry) FinishDelivery(i int, at time.Time, err error) bool {
	if i < 0 || i >= len(e.Deliveries) || e.Deliveries[i].Status != "pending" {
		return false
	}
	d := &e.Deliveries[i]
	d.FinishedAt = at
	d.Status = "sent"
	if err != nil {
		d.Status = "failed"
		if n := len(d.Attempts); n == 0 || d.Attempts[n-1].Error == "" {
			d.Attempts = append(d.Attempts, AuditAttempt{Time: at, Error: err.Error()}) // failed before any send
		}
	}
	if e.Verdict != AuditPending {
		return false
	}
	verdict, settled := e.settledVerdict()
	if !settled {
		return false
	}
	e.Verdict = verdict
	return true
}

// settledVerdict is the verdict once no delivery is pending any more.
func (e *AuditEntry) settledVerdict() (string, bool) {
	rescued := map[int]bool{}
	for _, d := range e.Deliveries {
		switch {
		case d.Status == "pending":
			return "", false
		case d.Status == "sent" && d.FallbackFor != nil:
			rescued[*d.FallbackFor] = true
		}
	}
	verdict := AuditDelivered
	for i, d := range e.Deliveries {
		if d.Status != "failed" || d.FallbackFor != nil {
			continue // a failed fallback counts through the delivery it stood in for
		}
		if !rescued[i] {
			return AuditFailed, true
		}
		verdict = AuditFallback
	}
	return verdict, true
}

// Matches reports whether the entry fits the query's filters.
func (e AuditEntry) Matches(q AuditQuery) bool {
	if q.Source != "" && e.Source != q.Source {
		return false
	}
	if q.Verdict != "" && e.Verdict != q.Verdict {
		return false
	}
	if q.Text == "" {
		return true
	}
	t := strings.ToLower(q.Text)
	hay := []string{e.ID, e.Source, e.Payload, e.Reason, e.DroppedBy, e.Heartbeat}
	for k, v := range e.Labels {
		hay = append(hay, k+"="+v)
	}
	for _, d := range e.Deliveries {
		hay = append(hay, d.Route, d.Destination, d.Message)
		for _, a := range d.Attempts {
			hay = append(hay, a.Error)
		}
	}
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), t) {
			return true
		}
	}
	return false
}

// Clone returns a deep copy, stores hand out copies only.
func (e AuditEntry) Clone() AuditEntry {
	c := e
	if e.Labels != nil {
		c.Labels = make(map[string]string, len(e.Labels))
		for k, v := range e.Labels {
			c.Labels[k] = v
		}
	}
	c.Trace = append([]domain.TraceStep(nil), e.Trace...)
	c.Deliveries = make([]AuditDelivery, len(e.Deliveries))
	for i, d := range e.Deliveries {
		d.Attempts = append([]AuditAttempt(nil), d.Attempts...)
		if d.FallbackFor != nil {
			v := *d.FallbackFor
			d.FallbackFor = &v
		}
		c.Deliveries[i] = d
	}
	if len(e.Deliveries) == 0 {
		c.Deliveries = nil
	}
	return c
}
