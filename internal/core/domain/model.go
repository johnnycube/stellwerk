// Package domain is the heart of stellwerk: events, rules, routing and
// rendering. It has no I/O and knows nothing about HTTP, YAML, Matrix or
// clustering. Everything else plugs in through internal/core/ports.
package domain

import (
	"strconv"
	"strings"
	"time"
)

// Event is the normalized form of everything a source delivers.
type Event struct {
	Source string            // configured source name, e.g. "alertmanager", "sonarr"
	Kind   string            // decoder kind, e.g. "alertmanager", "generic"
	Status string            // optional state, e.g. "firing"/"resolved"
	Labels map[string]string // flat key/values for matching
	Data   any               // JSON-like payload: map[string]any, []any, string, json.Number ...
	Key    string            // optional stable identity for dedupe, empty = hash of the raw body
}

// Priorities, lowest first. Targets map them to what they have (ntfy
// priority, mail headers). Later also the order in which queues are served.
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
	PriorityUrgent = "urgent"
)

// Priorities lists the valid priorities, lowest first.
var Priorities = []string{PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent}

// ValidPriority reports whether p is empty or one of Priorities.
func ValidPriority(p string) bool {
	if p == "" {
		return true
	}
	for _, x := range Priorities {
		if p == x {
			return true
		}
	}
	return false
}

// Message is what an outbound endpoint receives.
type Message struct {
	Markdown string
	Loud     bool     // should notify / push
	Priority string   // one of Priorities, never empty once routed
	Mentions []string // endpoint-specific identities (Matrix user IDs, ...)
	Source   string
	Route    string
}

// Destination is a named place: an account of a target plus an address.
type Destination struct {
	Alias   string
	Target  string // target instance, e.g. "matrix"
	Account string // account within the target, e.g. "alerts"
	Address string // room ID, URL path, topic ...
	// Fallback destinations get the message when delivery here finally
	// fails (all retries used). One level only: a fallback's own fallback
	// is not used.
	Fallback []string
}

// Endpoint identifies the sender that delivers to this destination.
func (d Destination) Endpoint() string { return EndpointID(d.Target, d.Account) }

func EndpointID(target, account string) string { return target + "/" + account }

// Delivery is one message headed to one destination.
type Delivery struct {
	Route       string
	Destination string // alias
	Endpoint    string // target/account
	Address     string
	Message     Message
	// EntryID and Index tie the delivery to its audit entry, set by the
	// application. Empty EntryID: not audited.
	EntryID string
	Index   int
	// ID identifies the delivery while it is queued or spooled. Set by the
	// application when it is enqueued.
	ID string
	// Fallback deliveries, built from the destination's fallback list.
	Fallback []Delivery
	// FallbackFor is the destination alias this delivery stands in for.
	FallbackFor string
	// Digest > 0: the message waits and goes out with the others of the
	// same route and destination as one summary after this window.
	Digest time.Duration
	// Created is when the delivery was first accepted. A fallback keeps the
	// time of the delivery it stands in for. Used to mark late messages.
	Created time.Time
}

// Rule is one routing rule. Rules are checked in order.
type Rule struct {
	Name     string
	Match    Match
	Expr     string // optional expression, ANDed with Match
	Engine   string // engine for Expr, empty = the default (cel)
	To       []string
	Template string
	Loud     bool
	Priority string        // empty: high if Loud, else normal
	Digest   time.Duration // > 0: collect messages and send one summary per window
	Mention  []string
	Drop     bool
	Continue bool
}

// Match: all given conditions must hold. Values are anchored regexes, a
// leading "!" negates. Source is a glob.
type Match struct {
	Source string            `json:"source,omitempty"`
	Status string            `json:"status,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Heartbeat is an event that must keep arriving.
type Heartbeat struct {
	Name    string
	Source  string
	Labels  map[string]string
	Timeout time.Duration
	To      []string
	Mention []string
}

// Matches reports whether ev is this heartbeat.
func (h Heartbeat) Matches(ev *Event) bool {
	if h.Source != ev.Source {
		return false
	}
	for k, v := range h.Labels {
		if ev.Labels[k] != v {
			return false
		}
	}
	return true
}

// DeliveryPolicy controls how the application delivers.
type DeliveryPolicy struct {
	Sync         bool // answer only after delivery
	Timeout      time.Duration
	QueueSize    int
	MaxRetries   int
	MaxBackoff   time.Duration
	DedupeWindow time.Duration
	// LateAfter: a message sent later than this after it was accepted gets
	// a note saying how late it is. 0 = never.
	LateAfter time.Duration
	// MaxAge: a message older than this is not sent at all. 0 = no limit.
	MaxAge time.Duration
	// After BreakerFailures deliveries in a row finally failed, an endpoint
	// is skipped for BreakerCooldown: messages go to fallbacks at once
	// instead of waiting through all retries. 0 = no breaker.
	BreakerFailures int
	BreakerCooldown time.Duration
}

// Spec is everything the core needs to know, mapped from config by an adapter.
type Spec struct {
	Engine       string // default expression engine, for display
	Rules        []Rule
	Destinations map[string]Destination
	Heartbeats   []Heartbeat
	Templates    map[string]string
	Delivery     DeliveryPolicy
}

// Lookup resolves dot paths like "series.title" or "episodes.0.title".
func Lookup(data any, path string) (any, bool) {
	cur := data
	for _, part := range strings.Split(path, ".") {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[part]
			if !ok {
				return nil, false
			}
			cur = v
		case map[string]string:
			v, ok := c[part]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// FindHeartbeat returns the heartbeat an event is, if any.
func FindHeartbeat(specs []Heartbeat, ev *Event) (Heartbeat, bool) {
	for _, h := range specs {
		if h.Matches(ev) {
			return h, true
		}
	}
	return Heartbeat{}, false
}
