package webhook

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
	"go.yaml.in/yaml/v4"
)

func TestWebhookSenderContract(t *testing.T) {
	portstest.SenderContract(t, func(t *testing.T) portstest.SenderUnderTest {
		var mu sync.Mutex
		var got []portstest.Sent
		var broken atomic.Bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if broken.Load() {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			var p Payload
			_ = json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			got = append(got, portstest.Sent{Address: r.URL.Path, Msg: domain.Message{Markdown: p.Markdown, Loud: p.Loud, Mentions: p.Mentions, Source: p.Source, Route: p.Route}})
			mu.Unlock()
		}))
		t.Cleanup(srv.Close)
		var n yaml.Node
		_ = yaml.Unmarshal([]byte("url: "+srv.URL), &n)
		s, err := target.New(context.Background(), "webhook", target.Env{Target: "hook", Account: "default", NodeID: "a", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, n.Content[0])
		if err != nil {
			t.Fatal(err)
		}
		return portstest.SenderUnderTest{
			Sender:   s,
			Received: func() []portstest.Sent { mu.Lock(); defer mu.Unlock(); return append([]portstest.Sent(nil), got...) },
			Break:    func() { broken.Store(true) },
		}
	})
}
