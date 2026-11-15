package scheduler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSelfPing(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.URL.Path+"|"+string(b))
		mu.Unlock()
		if r.URL.Path == "/broken" {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	healthy := true
	var hmu sync.Mutex
	p := &SelfPing{
		URL: srv.URL + "/uuid", Every: 20 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Health: func() (bool, string) { hmu.Lock(); defer hmu.Unlock(); return healthy, "matrix/alerts not ready" },
	}
	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)
	time.Sleep(30 * time.Millisecond)
	hmu.Lock()
	healthy = false
	hmu.Unlock()
	time.Sleep(50 * time.Millisecond)
	cancel()
	mu.Lock()
	seen := append([]string(nil), got...)
	mu.Unlock()
	if len(seen) < 3 || seen[0] != "/uuid|" || seen[len(seen)-1] != "/uuid/fail|matrix/alerts not ready" {
		t.Errorf("%v", seen)
	}
	// custom fail URL, broken server and bad URLs are logged, not fatal
	p2 := &SelfPing{URL: srv.URL + "/broken", FailURL: srv.URL + "/f", Health: func() (bool, string) { return true, "" }, Log: p.Log}
	p2.Client = srv.Client()
	p2.ping(context.Background())
	p3 := &SelfPing{URL: "http://127.0.0.1:1", Health: func() (bool, string) { return true, "" }, Log: p.Log, Client: srv.Client()}
	p3.ping(context.Background())
	p4 := &SelfPing{URL: "://bad", Health: func() (bool, string) { return true, "" }, Log: p.Log, Client: srv.Client()}
	p4.ping(context.Background())
}
