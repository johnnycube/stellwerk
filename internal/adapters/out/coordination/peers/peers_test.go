package peers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

// swap lets us create servers before the handler (which needs the URLs) exists.
type swap struct {
	mu sync.RWMutex
	h  http.Handler
}

func (s *swap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	h := s.h
	s.mu.RUnlock()
	if h == nil {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

func (s *swap) set(h http.Handler) { s.mu.Lock(); s.h = h; s.mu.Unlock() }

func startCluster(t *testing.T, ids ...string) (map[string]*Peers, map[string]*swap, context.CancelFunc) {
	t.Helper()
	swaps, urls := map[string]*swap{}, map[string]string{}
	for _, id := range ids {
		s := &swap{}
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		swaps[id], urls[id] = s, srv.URL
	}
	var peers []Peer
	for _, id := range ids {
		peers = append(peers, Peer{ID: id, URL: urls[id]})
	}
	ctx, cancel := context.WithCancel(context.Background())
	nodes := map[string]*Peers{}
	for _, id := range ids {
		p := New(Settings{NodeID: id, Secret: "s3cret", Interval: 50 * time.Millisecond, Peers: peers}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		swaps[id].set(p.Handler())
		nodes[id] = p
		go p.Run(ctx)
	}
	return nodes, swaps, cancel
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestLeaderElectionAndFailover(t *testing.T) {
	nodes, swaps, cancel := startCluster(t, "a", "b", "c")
	defer cancel()
	eventually(t, "a leads everywhere", func() bool {
		return nodes["a"].IsLeader() && !nodes["b"].IsLeader() && !nodes["c"].IsLeader()
	})
	swaps["a"].set(nil) // a goes down
	eventually(t, "b takes over", func() bool { return nodes["b"].IsLeader() && !nodes["c"].IsLeader() })
	st := nodes["b"].Status()
	if st.Leader != "b" || st.Peers[0].Alive || st.Peers[0].ID != "a" {
		t.Errorf("status: %+v", st)
	}
	swaps["a"].set(nodes["a"].Handler()) // a returns
	eventually(t, "a leads again", func() bool { return !nodes["b"].IsLeader() && nodes["c"].Status().Leader == "a" })
}

func TestHeartbeatSharing(t *testing.T) {
	nodes, swaps, cancel := startCluster(t, "a", "b")
	defer cancel()
	at := time.Now().Truncate(time.Millisecond)
	nodes["b"].SetHeartbeat("watchdog", at)
	eventually(t, "push reaches a", func() bool { got, ok := nodes["a"].Heartbeat("watchdog"); return ok && got.Equal(at) })

	// a misses a push while down, the state poll repairs it
	swaps["a"].set(nil)
	later := at.Add(time.Minute)
	nodes["b"].SetHeartbeat("watchdog", later)
	time.Sleep(100 * time.Millisecond)
	swaps["a"].set(nodes["a"].Handler())
	eventually(t, "anti-entropy", func() bool { got, _ := nodes["a"].Heartbeat("watchdog"); return got.Equal(later) })

	nodes["a"].SetHeartbeat("watchdog", at) // older time never wins
	if got, _ := nodes["a"].Heartbeat("watchdog"); !got.Equal(later) {
		t.Error("older heartbeat overwrote newer one")
	}
}

func TestDedupeSharing(t *testing.T) {
	nodes, _, cancel := startCluster(t, "a", "b")
	defer cancel()
	if !nodes["a"].Claim("k1", time.Minute) || nodes["a"].Claim("k1", time.Minute) {
		t.Fatal("local claim semantics broken")
	}
	eventually(t, "b knows k1", func() bool { return nodes["b"].Seen("k1") })
	if nodes["b"].Seen("k2") {
		t.Error("unknown key reported as seen")
	}
}

func TestAuthRequired(t *testing.T) {
	nodes, _, cancel := startCluster(t, "a")
	defer cancel()
	rec := httptest.NewRecorder()
	nodes["a"].Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/cluster/v1/state", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rec.Code)
	}
}

func TestPeersCoordinatorContract(t *testing.T) {
	portstest.CoordinatorContract(t, func(t *testing.T) portstest.CoordinatorUnderTest {
		p := New(Settings{
			NodeID: "a", Secret: "s", Interval: 50 * time.Millisecond,
			Peers: []Peer{{ID: "a", URL: "http://127.0.0.1:1"}, {ID: "b", URL: "http://127.0.0.1:2"}},
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		return portstest.CoordinatorUnderTest{Coordinator: p, Advance: time.Sleep}
	})
}
