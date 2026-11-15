// Package peers is the high-availability coordinator: N identical nodes with
// a static peer list and no extra infrastructure.
//
//   - Heartbeat times are pushed to all peers immediately and re-synced every
//     interval, so a heartbeat that hit node A is known on node B.
//   - The leader is the live node with the lowest ID. Only the leader raises
//     heartbeat alerts. If it dies, the next one takes over within a few intervals.
//   - Dedupe keys are shared best effort, so a sender that retries against
//     another node does not produce a second message.
//
// Semantics are at-least-once: during a network split two nodes may both
// lead and both alert. A duplicate is better than silence.
package peers

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/out/coordination/memstate"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

type peer struct {
	id, url  string
	lastOK   time.Time
	lastErr  string
	reported string // id the peer reported, to catch miswired URLs
}

// Settings configures the peers coordinator.
type Settings struct {
	NodeID   string
	Secret   string
	Interval time.Duration
	Peers    []Peer // all nodes incl. this one
}

type Peer struct{ ID, URL string }

type Peers struct {
	id       string
	secret   string
	interval time.Duration
	st       *memstate.State
	client   *http.Client
	log      *slog.Logger

	mu    sync.Mutex
	peers []*peer // other nodes only
}

var _ ports.Coordinator = (*Peers)(nil)

func New(cfg Settings, log *slog.Logger) *Peers {
	nodeID := cfg.NodeID
	p := &Peers{id: nodeID, secret: cfg.Secret, interval: cfg.Interval, st: memstate.New(),
		client: &http.Client{Timeout: cfg.Interval}, log: log.With("component", "cluster")}
	for _, c := range cfg.Peers {
		if c.ID != nodeID {
			p.peers = append(p.peers, &peer{id: c.ID, url: strings.TrimRight(c.URL, "/")})
		}
	}
	return p
}

func (p *Peers) NodeID() string { return p.id }

func (p *Peers) alive(pe *peer, now time.Time) bool {
	return !pe.lastOK.IsZero() && now.Sub(pe.lastOK) < 3*p.interval
}

// IsLeader: lowest ID among live nodes, this node is always live to itself.
func (p *Peers) IsLeader() bool { return p.leader() == p.id }

func (p *Peers) leader() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	leader := p.id
	for _, pe := range p.peers {
		if p.alive(pe, now) && pe.id < leader {
			leader = pe.id
		}
	}
	return leader
}

func (p *Peers) SetHeartbeat(name string, at time.Time) {
	if p.st.SetHeartbeat(name, at) {
		p.broadcast("/cluster/v1/heartbeat", heartbeatMsg{Name: name, At: at})
	}
}

func (p *Peers) Heartbeat(name string) (time.Time, bool) { return p.st.Heartbeat(name) }

func (p *Peers) Claim(key string, ttl time.Duration) bool {
	if !p.st.Claim(key, ttl) {
		return false
	}
	p.broadcast("/cluster/v1/seen", seenMsg{Key: key, Expires: time.Now().Add(ttl)})
	return true
}

func (p *Peers) Seen(key string) bool { return p.st.Seen(key) }

func (p *Peers) Status() ports.ClusterStatus {
	leader := p.leader()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	st := ports.ClusterStatus{Enabled: true, NodeID: p.id, Leader: leader,
		Peers: []ports.PeerStatus{{ID: p.id, Self: true, Alive: true, LastSeen: now}}}
	for _, pe := range p.peers {
		st.Peers = append(st.Peers, ports.PeerStatus{ID: pe.id, URL: pe.url, Alive: p.alive(pe, now), LastSeen: pe.lastOK, Error: pe.lastErr})
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].ID < st.Peers[j].ID })
	return st
}

// Run polls every peer's state each interval. That is both the liveness
// check and anti-entropy for heartbeats a push may have missed.
func (p *Peers) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	p.pollAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollAll(ctx)
		}
	}
}

func (p *Peers) pollAll(ctx context.Context) {
	p.mu.Lock()
	peers := append([]*peer(nil), p.peers...)
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, pe := range peers {
		wg.Add(1)
		go func(pe *peer) {
			defer wg.Done()
			st, err := p.fetchState(ctx, pe)
			p.mu.Lock()
			defer p.mu.Unlock()
			if err != nil {
				if pe.lastErr != err.Error() {
					p.log.Warn("peer unreachable", "peer", pe.id, "err", err)
				}
				pe.lastErr = err.Error()
				return
			}
			if st.ID != pe.id {
				pe.lastErr = fmt.Sprintf("peer at %s reports id %q, expected %q", pe.url, st.ID, pe.id)
				return
			}
			if pe.lastErr != "" || pe.lastOK.IsZero() {
				p.log.Info("peer up", "peer", pe.id)
			}
			pe.lastOK, pe.lastErr = time.Now(), ""
			for name, at := range st.Heartbeats {
				p.st.SetHeartbeat(name, at)
			}
		}(pe)
	}
	wg.Wait()
}

type stateMsg struct {
	ID         string               `json:"id"`
	Heartbeats map[string]time.Time `json:"heartbeats"`
}

type heartbeatMsg struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

type seenMsg struct {
	Key     string    `json:"key"`
	Expires time.Time `json:"expires"`
}

func (p *Peers) fetchState(ctx context.Context, pe *peer) (stateMsg, error) {
	var st stateMsg
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, pe.url+"/cluster/v1/state", nil)
	req.Header.Set("Authorization", "Bearer "+p.secret)
	resp, err := p.client.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("state: %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// broadcast pushes a message to all peers in the background. Failures are
// fine: the state poll repairs heartbeats, dedupe is best effort.
func (p *Peers) broadcast(path string, msg any) {
	body, _ := json.Marshal(msg)
	p.mu.Lock()
	peers := append([]*peer(nil), p.peers...)
	p.mu.Unlock()
	for _, pe := range peers {
		go func(url string) {
			ctx, cancel := context.WithTimeout(context.Background(), p.interval)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url+path, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+p.secret)
			req.Header.Set("Content-Type", "application/json")
			if resp, err := p.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}(pe.url)
	}
}

func (p *Peers) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cluster/v1/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stateMsg{ID: p.id, Heartbeats: p.st.Heartbeats()})
	})
	mux.HandleFunc("POST /cluster/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var m heartbeatMsg
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.st.SetHeartbeat(m.Name, m.At)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /cluster/v1/seen", func(w http.ResponseWriter, r *http.Request) {
		var m seenMsg
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.st.MarkSeen(m.Key, m.Expires)
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(p.secret)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
