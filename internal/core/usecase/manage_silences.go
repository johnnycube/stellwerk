package usecase

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// ErrNotFound: no silence or audit entry with that ID.
var ErrNotFound = errors.New("not found")

// Silencer is what routing needs from silences.
type Silencer interface {
	Find(ev *domain.Event) (domain.Silence, bool)
}

// ManageSilences implements ports.ManageSilences and Silencer. Silences are
// kept in memory, compiled, and written through to the store so they
// survive a restart.
type ManageSilences struct {
	store ports.SilenceStore
	clock ports.Clock
	log   *slog.Logger

	mu       sync.RWMutex
	silences []silence
}

type silence struct {
	s domain.Silence
	m interface {
		Explain(*domain.Event) (bool, string)
	}
}

var (
	_ ports.ManageSilences = (*ManageSilences)(nil)
	_ Silencer             = (*ManageSilences)(nil)
)

// MaxSilence is the longest a silence may last, so nothing stays muted and
// forgotten.
const MaxSilence = 30 * 24 * time.Hour

func NewManageSilences(store ports.SilenceStore, clock ports.Clock, log *slog.Logger) *ManageSilences {
	u := &ManageSilences{store: store, clock: clock, log: log}
	all, err := store.All()
	if err != nil {
		log.Error("silences could not be loaded", "err", err)
	}
	now := clock.Now()
	for _, s := range all {
		if !now.Before(s.Ends) {
			_ = store.Delete(s.ID) // expired while we were down
			continue
		}
		m, err := domain.CompileMatch(s.Match)
		if err != nil {
			log.Error("stored silence is invalid, dropped", "id", s.ID, "err", err)
			_ = store.Delete(s.ID)
			continue
		}
		u.silences = append(u.silences, silence{s, m})
	}
	return u
}

func (u *ManageSilences) Silence(req ports.SilenceRequest) (domain.Silence, error) {
	if req.ID != "" {
		u.mu.RLock()
		for _, s := range u.silences {
			if s.s.ID == req.ID {
				u.mu.RUnlock()
				return s.s, nil
			}
		}
		u.mu.RUnlock()
	}
	m, err := domain.ParseMatchers(req.Matchers)
	if err != nil {
		return domain.Silence{}, err
	}
	d, err := domain.ParseDuration(req.For)
	if err != nil {
		return domain.Silence{}, err
	}
	if d > MaxSilence {
		return domain.Silence{}, fmt.Errorf("at most 30 days")
	}
	cm, _ := domain.CompileMatch(m) // ParseMatchers compiled it already
	now := u.clock.Now()
	id := req.ID
	if id == "" {
		id = silenceID()
	}
	s := domain.Silence{ID: id, Match: m, Starts: now, Ends: now.Add(d), CreatedBy: req.By, Comment: strings.TrimSpace(req.Comment)}
	if err := u.store.Save(s); err != nil {
		return domain.Silence{}, fmt.Errorf("silence not stored: %w", err)
	}
	u.mu.Lock()
	u.silences = append(u.prune(now), silence{s, cm})
	u.mu.Unlock()
	u.log.Info("silence added", "id", s.ID, "match", s.Describe(), "until", s.Ends, "by", s.CreatedBy)
	return s, nil
}

// Expire accepts the full ID or a unique prefix of it, chat users type less.
func (u *ManageSilences) Expire(id string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	idx := -1
	for i, s := range u.silences {
		if s.s.ID == id || (id != "" && strings.HasPrefix(s.s.ID, id)) {
			if idx >= 0 && u.silences[idx].s.ID != id {
				return fmt.Errorf("%q matches more than one silence", id)
			}
			idx = i
		}
	}
	if idx < 0 {
		return fmt.Errorf("silence %q: %w", id, ErrNotFound)
	}
	if err := u.store.Delete(u.silences[idx].s.ID); err != nil {
		return err
	}
	u.log.Info("silence ended", "id", u.silences[idx].s.ID)
	u.silences = append(u.silences[:idx], u.silences[idx+1:]...)
	return nil
}

func (u *ManageSilences) Active() []domain.Silence {
	now := u.clock.Now()
	u.mu.RLock()
	defer u.mu.RUnlock()
	var out []domain.Silence
	for _, s := range u.silences {
		if s.s.Active(now) {
			out = append(out, s.s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ends.Before(out[j].Ends) })
	return out
}

// Find returns the first active silence that matches ev.
func (u *ManageSilences) Find(ev *domain.Event) (domain.Silence, bool) {
	now := u.clock.Now()
	u.mu.RLock()
	defer u.mu.RUnlock()
	for _, s := range u.silences {
		if s.s.Active(now) {
			if ok, _ := s.m.Explain(ev); ok {
				return s.s, true
			}
		}
	}
	return domain.Silence{}, false
}

// prune drops expired silences, caller holds the lock.
func (u *ManageSilences) prune(now time.Time) []silence {
	keep := u.silences[:0]
	for _, s := range u.silences {
		if now.Before(s.s.Ends) {
			keep = append(keep, s)
		} else {
			_ = u.store.Delete(s.s.ID)
		}
	}
	return keep
}

func silenceID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "s-" + hex.EncodeToString(b)
}
