//go:build process

// Package process tests the real binary: it is killed with SIGKILL while a
// delivery is still failing, started again, and must deliver what it
// accepted before.
//
//	go test -tags "goolm process" -v ./test/process
package process

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func build(t *testing.T) string {
	bin := filepath.Join(t.TempDir(), "stellwerk")
	cmd := exec.Command("go", "build", "-tags", "goolm", "-o", bin, "../../cmd/stellwerk")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return bin
}

func start(t *testing.T, bin, cfg string) *exec.Cmd {
	cmd := exec.Command(bin, "-config", cfg)
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitReady(t *testing.T, addr string) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := http.Get("http://" + addr + "/readyz"); err == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("stellwerk did not become ready")
}

func TestKilledMidDeliveryResumesAfterRestart(t *testing.T) {
	bin := build(t)
	var up atomic.Bool
	var mu sync.Mutex
	var got []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
	}))
	defer receiver.Close()

	dir := t.TempDir()
	addr := freePort(t)
	cfg := filepath.Join(dir, "stellwerk.yaml")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`
node_id: p
listen: %q
sources: { app: { kind: generic, token: t } }
targets: { hook: { type: webhook, settings: { url: %q } } }
destinations: { out: { target: hook } }
routes: [ { to: out } ]
delivery: { mode: async, max_retries: 50, max_backoff: 200ms, spool: %q, breaker_failures: -1 }
ui: { listen: "" }
`, addr, receiver.URL, filepath.Join(dir, "spool.db"))), 0o600)

	first := start(t, bin, cfg)
	waitReady(t, addr)
	for i := range 3 {
		req, _ := http.NewRequest("POST", "http://"+addr+"/in/app", strings.NewReader(fmt.Sprintf(`{"title":"keep %d"}`, i)))
		req.Header.Set("Authorization", "Bearer t")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	time.Sleep(500 * time.Millisecond) // retrying against the broken receiver
	if err := first.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = first.Wait()

	up.Store(true)
	second := start(t, bin, cfg)
	defer func() { second.Process.Signal(syscall.SIGTERM); second.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("delivered %d of 3 after the restart: %v", len(got), got)
	}
	for i := range 3 {
		if !strings.Contains(strings.Join(got, "\n"), fmt.Sprintf("keep %d", i)) {
			t.Errorf("message %d lost", i)
		}
	}
}
