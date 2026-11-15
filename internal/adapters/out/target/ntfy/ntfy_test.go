package ntfy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
	"go.yaml.in/yaml/v3"
)

type server struct {
	*httptest.Server
	mu     sync.Mutex
	got    []Publish
	auth   []string
	broken atomic.Bool
}

func newServer(t *testing.T) *server {
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.broken.Load() {
			http.Error(w, `{"error":"limit reached"}`, http.StatusTooManyRequests)
			return
		}
		var p Publish
		_ = json.NewDecoder(r.Body).Decode(&p)
		s.mu.Lock()
		s.got = append(s.got, p)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) published() []Publish {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Publish(nil), s.got...)
}

func newNtfy(t *testing.T, yml string) target.Target {
	t.Helper()
	var n yaml.Node
	_ = yaml.Unmarshal([]byte(yml), &n)
	x, err := target.New(context.Background(), "ntfy", target.Env{Target: "push", Account: "default", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, n.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestNtfySenderContract(t *testing.T) {
	portstest.SenderContract(t, func(t *testing.T) portstest.SenderUnderTest {
		s := newServer(t)
		return portstest.SenderUnderTest{
			Sender: newNtfy(t, "url: "+s.URL),
			Received: func() []portstest.Sent {
				var out []portstest.Sent
				for _, p := range s.published() {
					md := p.Message
					if p.Title != "" {
						md = p.Title + "\n" + p.Message
					}
					out = append(out, portstest.Sent{Address: p.Topic, Msg: domain.Message{Markdown: md}})
				}
				return out
			},
			Break:       func() { s.broken.Store(true) },
			AddressHint: "alerts",
			BodyOnly:    true,
		}
	})
}

func TestNtfyPublish(t *testing.T) {
	s := newServer(t)
	n := newNtfy(t, "url: "+s.URL+"/\ntoken: tk_abc\ntags: [stellwerk]")
	msg := domain.Message{Markdown: "🔥 **Disk full** FIRING\n- node a\n- node b", Priority: domain.PriorityUrgent}
	if err := n.Send(context.Background(), "alerts", msg); err != nil {
		t.Fatal(err)
	}
	p := s.published()[0]
	if p.Topic != "alerts" || p.Title != "🔥 Disk full FIRING" || p.Message != "- node a\n- node b" || p.Priority != 5 || !p.Markdown || p.Tags[0] != "stellwerk" {
		t.Errorf("%+v", p)
	}
	if s.auth[0] != "Bearer tk_abc" {
		t.Errorf("auth %q", s.auth[0])
	}
	basic := newNtfy(t, "url: "+s.URL+"\nusername: u\npassword: p")
	_ = basic.Send(context.Background(), "t", domain.Message{Markdown: "one line", Loud: true})
	p = s.published()[1]
	if s.auth[1] != "Basic dTpw" || p.Title != "" || p.Message != "one line" || p.Priority != 4 {
		t.Errorf("%+v %q", p, s.auth[1])
	}
}

func TestNtfyErrors(t *testing.T) {
	s := newServer(t)
	s.broken.Store(true)
	n := newNtfy(t, "url: "+s.URL)
	if err := n.Send(context.Background(), "t", domain.Message{Markdown: "x"}); err == nil || err.Error() != `ntfy answered 429 Too Many Requests: {"error":"limit reached"}` {
		t.Errorf("%v", err)
	}
	if err := n.Send(context.Background(), " ", domain.Message{}); err == nil {
		t.Error("empty topic")
	}
	env := target.Env{Target: "push", Account: "default"}
	if _, err := New(env, Settings{URL: "ftp://x"}); err == nil {
		t.Error("bad url")
	}
	if _, err := New(env, Settings{Token: "t", Username: "u"}); err == nil {
		t.Error("token and username")
	}
	if x, _ := New(env, Settings{}); x.s.URL != "https://ntfy.sh" {
		t.Error("default url")
	}
	down := newNtfy(t, "url: http://127.0.0.1:1")
	if err := down.Send(context.Background(), "t", domain.Message{Markdown: "x"}); err == nil {
		t.Error("connection refused")
	}
	bad := &Ntfy{s: Settings{URL: "http://a b"}, client: http.DefaultClient}
	if err := bad.Send(context.Background(), "t", domain.Message{}); err == nil {
		t.Error("bad request url")
	}
}
