package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v3"
)

func TestWebhookTarget(t *testing.T) {
	var got Payload
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		if r.URL.Path == "/fail" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	var n yaml.Node
	_ = yaml.Unmarshal([]byte("url: "+srv.URL+"\nheaders: { Authorization: Bearer abc }"), &n)
	tg, err := target.New(context.Background(), "webhook", target.Env{Target: "hook", Account: "ops", NodeID: "a", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, n.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	msg := domain.Message{Markdown: "**hi**", Loud: true, Source: "am", Route: "r1", Mentions: []string{"x"}}
	if err := tg.Send(context.Background(), "/alerts", msg); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer abc" || path != "/alerts" || got.Markdown != "**hi**" || !got.Loud || got.Node != "a" || got.Account != "ops" || got.Route != "r1" {
		t.Errorf("auth=%q path=%q payload=%+v", auth, path, got)
	}
	if err := tg.Send(context.Background(), "/fail", msg); err == nil {
		t.Error("expected error on 500")
	}
}
