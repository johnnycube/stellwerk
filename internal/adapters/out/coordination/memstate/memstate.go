// Package memstate is the in-memory state shared by the coordination adapters.
package memstate

import (
	"sync"
	"time"
)

type State struct {
	mu         sync.Mutex
	heartbeats map[string]time.Time
	seen       map[string]time.Time // key -> expiry
	Now        func() time.Time
}

func New() *State {
	return &State{heartbeats: map[string]time.Time{}, seen: map[string]time.Time{}, Now: time.Now}
}

// SetHeartbeat keeps the newest time and reports whether it changed.
func (s *State) SetHeartbeat(name string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.After(s.heartbeats[name]) {
		s.heartbeats[name] = at
		return true
	}
	return false
}

func (s *State) Heartbeat(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.heartbeats[name]
	return t, ok
}

func (s *State) Heartbeats() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time, len(s.heartbeats))
	for k, v := range s.heartbeats {
		out[k] = v
	}
	return out
}

func (s *State) Seen(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.seen[key]
	return ok && exp.After(s.Now())
}

// Claim marks key until ttl and reports whether it was new.
func (s *State) Claim(key string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if exp, ok := s.seen[key]; ok && exp.After(now) {
		return false
	}
	s.seen[key] = now.Add(ttl)
	if len(s.seen) > 10000 {
		for k, exp := range s.seen {
			if !exp.After(now) {
				delete(s.seen, k)
			}
		}
	}
	return true
}

func (s *State) MarkSeen(key string, exp time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if exp.After(s.seen[key]) {
		s.seen[key] = exp
	}
}
