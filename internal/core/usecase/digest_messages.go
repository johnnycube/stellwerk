package usecase

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// digestMaxChars keeps a summary below what chat clients show comfortably.
const digestMaxChars = 10000

// DigestMessages sits in front of DeliverMessages. Deliveries of routes with
// a digest window are collected per route and destination and go out as one
// summary when the window ends. Everything else passes straight through.
//
// Collected messages are held in memory: a crash loses the open digest,
// a clean shutdown sends it early (FlushAll). Digests are meant for
// low-priority noise, urgent routes should not use them.
type DigestMessages struct {
	next  Enqueuer
	clock ports.Clock
	log   *slog.Logger
	audit ports.AuditStore

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	first time.Time
	items []domain.Delivery
}

var _ Enqueuer = (*DigestMessages)(nil)

func NewDigestMessages(next Enqueuer, clock ports.Clock, log *slog.Logger) *DigestMessages {
	return &DigestMessages{next: next, clock: clock, log: log, buckets: map[string]*bucket{}}
}

// WithAudit finishes the audit deliveries of collected messages when their
// digest was sent.
func (u *DigestMessages) WithAudit(store ports.AuditStore) *DigestMessages {
	u.audit = store
	return u
}

func (u *DigestMessages) Enqueue(d domain.Delivery) (<-chan error, error) {
	if d.Digest <= 0 {
		return u.next.Enqueue(d)
	}
	key := d.Route + "\x00" + d.Destination
	u.mu.Lock()
	b, ok := u.buckets[key]
	if !ok {
		b = &bucket{first: u.clock.Now()}
		u.buckets[key] = b
	}
	b.items = append(b.items, d)
	u.mu.Unlock()
	done := make(chan error, 1)
	done <- nil // accepted, the summary goes out later
	return done, nil
}

// Waiting returns how many messages wait in open digests.
func (u *DigestMessages) Waiting() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, b := range u.buckets {
		n += len(b.items)
	}
	return n
}

// Run sends digests whose window ended, until ctx ends.
func (u *DigestMessages) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.Flush(false)
		}
	}
}

// FlushAll sends every open digest now, e.g. on shutdown.
func (u *DigestMessages) FlushAll() { u.Flush(true) }

// Flush sends the digests that are due, or all with force.
func (u *DigestMessages) Flush(force bool) {
	now := u.clock.Now()
	u.mu.Lock()
	var due []*bucket
	keys := make([]string, 0, len(u.buckets))
	for k := range u.buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := u.buckets[k]
		if force || !now.Before(b.first.Add(b.items[0].Digest)) {
			due = append(due, b)
			delete(u.buckets, k)
		}
	}
	u.mu.Unlock()
	for _, b := range due {
		u.send(b, now)
	}
}

func (u *DigestMessages) send(b *bucket, now time.Time) {
	d := Digest(b.items, now.Sub(b.first))
	done, err := u.next.Enqueue(d)
	if err != nil {
		u.log.Error("digest could not be queued", "route", d.Route, "destination", d.Destination, "messages", len(b.items), "err", err)
		u.finish(b.items, err)
		return
	}
	go func() { u.finish(b.items, <-done) }()
}

func (u *DigestMessages) finish(items []domain.Delivery, err error) {
	if u.audit == nil {
		return
	}
	if err != nil {
		err = fmt.Errorf("digest: %w", err)
	}
	for _, it := range items {
		if it.EntryID != "" {
			u.audit.Finish(it.EntryID, it.Index, u.clock.Now(), err)
		}
	}
}

// Digest folds deliveries of one route and destination into one. The
// newest come last, the summary is cut at digestMaxChars.
func Digest(items []domain.Delivery, span time.Duration) domain.Delivery {
	first := items[0]
	out := first
	out.ID, out.EntryID, out.Index, out.Digest, out.Created = "", "", 0, 0, time.Time{}
	var sb strings.Builder
	fmt.Fprintf(&sb, "📬 **Digest**: %d %s from `%s` in the last %s", len(items), plural(len(items), "message", "messages"), first.Route, roundAge(span))
	rank := 0
	for i, it := range items {
		out.Message.Loud = out.Message.Loud || it.Message.Loud
		if r := domain.PriorityRank(it.Message.Priority); r > rank {
			rank = r
		}
		part := "\n\n---\n\n" + it.Message.Markdown
		if sb.Len()+len(part) > digestMaxChars {
			fmt.Fprintf(&sb, "\n\n---\n\n…and %d more, see the audit log.", len(items)-i)
			break
		}
		sb.WriteString(part)
	}
	out.Message.Markdown = sb.String()
	out.Message.Priority = domain.Priorities[rank]
	out.Message.Mentions = nil
	out.Fallback = nil
	for _, fb := range first.Fallback {
		fb.Message = out.Message
		fb.Message.Markdown += domain.FallbackNote(first.Destination)
		out.Fallback = append(out.Fallback, fb)
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
