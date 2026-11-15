//go:build e2e

// Package e2e runs stellwerk against a real Matrix homeserver.
//
//	STELLWERK_E2E_HOMESERVER=http://localhost:8008 \
//	STELLWERK_E2E_SHARED_SECRET=<registration_shared_secret> \
//	go test -tags "goolm e2e" -v ./test/e2e
//
// It registers fresh users, creates encrypted rooms as a human user with its
// own E2EE client, runs stellwerk with two bot accounts and checks that the
// human can decrypt what arrives, and that the audit log tells the truth.
package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	_ "github.com/johnnycube/stellwerk/internal/adapters/engine/all"
	_ "github.com/johnnycube/stellwerk/internal/adapters/out/target/all"
	"github.com/johnnycube/stellwerk/internal/bootstrap"
	"github.com/johnnycube/stellwerk/internal/config"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

type env struct {
	hs, secret, server string
}

func setup(t *testing.T) env {
	e := env{hs: os.Getenv("STELLWERK_E2E_HOMESERVER"), secret: os.Getenv("STELLWERK_E2E_SHARED_SECRET")}
	if e.hs == "" || e.secret == "" {
		t.Skip("set STELLWERK_E2E_HOMESERVER and STELLWERK_E2E_SHARED_SECRET")
	}
	resp, err := http.Get(e.hs + "/_matrix/client/versions")
	if err != nil {
		t.Fatalf("homeserver not reachable: %v", err)
	}
	resp.Body.Close()
	return e
}

func suffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// register creates a user with Synapse's shared-secret admin API.
func (e *env) register(t *testing.T, user, password string) id.UserID {
	t.Helper()
	var nonce struct {
		Nonce string `json:"nonce"`
	}
	getJSON(t, e.hs+"/_synapse/admin/v1/register", &nonce)
	mac := hmac.New(sha1.New, []byte(e.secret))
	mac.Write([]byte(nonce.Nonce + "\x00" + user + "\x00" + password + "\x00notadmin"))
	body, _ := json.Marshal(map[string]any{"nonce": nonce.Nonce, "username": user, "password": password, "admin": false, "mac": hex.EncodeToString(mac.Sum(nil))})
	resp, err := http.Post(e.hs+"/_synapse/admin/v1/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		UserID string `json:"user_id"`
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || json.Unmarshal(b, &out) != nil {
		t.Fatalf("register %s: %s %s", user, resp.Status, b)
	}
	if e.server == "" {
		e.server = strings.SplitN(out.UserID, ":", 2)[1]
	}
	return id.UserID(out.UserID)
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}

// human is the person reading the rooms, with a real E2EE client.
type human struct {
	cli *mautrix.Client
	mu  sync.Mutex
	got []received
}

type received struct {
	room      id.RoomID
	sender    id.UserID
	encrypted bool
	msgType   event.MessageType
	body      string
	mentions  []id.UserID
}

func newHuman(t *testing.T, ctx context.Context, hs string, user id.UserID, password string) *human {
	t.Helper()
	cli, err := mautrix.NewClient(hs, user, "")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := cryptohelper.NewCryptoHelper(cli, []byte("e2e"), filepath.Join(t.TempDir(), "human.db"))
	if err != nil {
		t.Fatal(err)
	}
	local, _, _ := user.Parse()
	ch.LoginAs = &mautrix.ReqLogin{Type: mautrix.AuthTypePassword, Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: local}, Password: password}
	if err := ch.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cli.Crypto = ch
	h := &human{cli: cli}
	cli.Syncer.(*mautrix.DefaultSyncer).OnEventType(event.EventMessage, func(_ context.Context, ev *event.Event) {
		mc := ev.Content.AsMessage()
		r := received{room: ev.RoomID, sender: ev.Sender, encrypted: ev.Mautrix.WasEncrypted, msgType: mc.MsgType, body: mc.Body}
		if mc.Mentions != nil {
			r.mentions = mc.Mentions.UserIDs
		}
		h.mu.Lock()
		h.got = append(h.got, r)
		h.mu.Unlock()
	})
	go func() { _ = cli.SyncWithContext(ctx) }()
	t.Cleanup(func() { ch.Close() })
	return h
}

func (h *human) room(t *testing.T, ctx context.Context, name string, encrypted bool, invite ...id.UserID) id.RoomID {
	t.Helper()
	req := &mautrix.ReqCreateRoom{Name: name, Preset: "private_chat", Invite: invite}
	if encrypted {
		req.InitialState = []*event.Event{{Type: event.StateEncryption, Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}}}}
	}
	resp, err := h.cli.CreateRoom(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.RoomID
}

func (h *human) waitFor(t *testing.T, what string, match func(received) bool) received {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, r := range h.got {
			if match(r) {
				h.mu.Unlock()
				return r
			}
		}
		h.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return received{}
}

func waitJoined(t *testing.T, ctx context.Context, cli *mautrix.Client, room id.RoomID, user id.UserID) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if m, err := cli.JoinedMembers(ctx, room); err == nil {
			if _, ok := m.Joined[user]; ok {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not join %s", user, room)
}

func TestStellwerkAgainstARealHomeserver(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := suffix()
	me := e.register(t, "human-"+s, "pw-human")
	alerts := e.register(t, "alerts-"+s, "pw-alerts")
	media := e.register(t, "media-"+s, "pw-media")

	h := newHuman(t, ctx, e.hs, me, "pw-human")
	critRoom := h.room(t, ctx, "critical", true, alerts)
	mediaRoom := h.room(t, ctx, "media", true, media)
	plainRoom := h.room(t, ctx, "plain", false, alerts)

	dir := t.TempDir()
	yml := fmt.Sprintf(`
node_id: e2e
listen: "127.0.0.1:0"
sources:
  alertmanager: { kind: alertmanager, token: am }
  sonarr: { kind: generic, token: so }
targets:
  matrix:
    type: matrix
    settings:
      homeserver: %q
      database: %q
      allowed_inviters: [%q]
    accounts:
      alerts: { user_id: %q, password: pw-alerts, pickle_key: k1 }
      media:  { user_id: %q, password: pw-media,  pickle_key: k2 }
destinations:
  critical: { target: matrix, account: alerts, address: %q }
  media:    { target: matrix, account: media,  address: %q }
  plain:    { target: matrix, account: alerts, address: %q }
routes:
  - { name: critical, match: { source: alertmanager, labels: { severity: critical } }, to: critical, loud: true, mention: [%q] }
  - { name: plain, match: { source: alertmanager, labels: { severity: info } }, to: plain }
  - { name: media, match: { source: sonarr }, to: media }
delivery: { mode: sync, timeout: 30s, max_retries: 2, max_backoff: 500ms }
ui: { listen: "" }
`, e.hs, filepath.Join(dir, "{account}.db"), me, alerts, media, critRoom, mediaRoom, plainRoom, me)
	cfg, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	app, err := bootstrap.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- app.Run(runCtx) }()
	defer func() { stop(); <-done }()

	srv := httptest.NewServer(app.HTTP.Handler())
	defer srv.Close()
	for !app.Ready() {
		time.Sleep(100 * time.Millisecond)
	}
	waitJoined(t, ctx, h.cli, critRoom, alerts)
	waitJoined(t, ctx, h.cli, mediaRoom, media)
	waitJoined(t, ctx, h.cli, plainRoom, alerts)

	post := func(path, token, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	alert := func(sev, summary string) string {
		return fmt.Sprintf(`{"version":"4","status":"firing","receiver":"r","groupLabels":{"alertname":"Disk"},"commonLabels":{"alertname":"Disk","severity":%q},"alerts":[{"status":"firing","labels":{"alertname":"Disk"},"annotations":{"summary":%q}}]}`, sev, summary)
	}

	t.Run("critical alert arrives encrypted, loud, with mention", func(t *testing.T) {
		if code := post("/in/alertmanager", "am", alert("critical", "disk on nas full "+s)); code != 200 {
			t.Fatalf("status %d", code)
		}
		r := h.waitFor(t, "critical message", func(r received) bool { return r.room == critRoom && strings.Contains(r.body, "disk on nas full "+s) })
		if !r.encrypted || r.sender != alerts || r.msgType != event.MsgText || len(r.mentions) != 1 || r.mentions[0] != me {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("second account posts quietly into its own room", func(t *testing.T) {
		if code := post("/in/sonarr", "so", `{"title":"Download","message":"Andor `+s+`"}`); code != 200 {
			t.Fatalf("status %d", code)
		}
		r := h.waitFor(t, "media message", func(r received) bool { return r.room == mediaRoom && strings.Contains(r.body, "Andor "+s) })
		if !r.encrypted || r.sender != media || r.msgType != event.MsgNotice {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("unencrypted rooms are refused and the audit log says why", func(t *testing.T) {
		if code := post("/in/alertmanager", "am", alert("info", "should never be sent "+s)); code != http.StatusBadGateway {
			t.Fatalf("want 502, got %d", code)
		}
		entries := app.ReadAudit.List(ports.AuditQuery{Verdict: ports.AuditFailed, Text: "should never be sent"})
		if len(entries) != 1 {
			t.Fatalf("audit entries %d", len(entries))
		}
		d := entries[0].Deliveries[0]
		if d.Status != "failed" || len(d.Attempts) != 2 || !strings.Contains(d.Attempts[0].Error, "not known as encrypted") {
			t.Errorf("delivery %+v", d)
		}
		time.Sleep(time.Second)
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, r := range h.got {
			if strings.Contains(r.body, "should never be sent") {
				t.Error("a message reached an unencrypted room")
			}
		}
	})

	t.Run("audit log has the delivered entries with their attempts", func(t *testing.T) {
		entries := app.ReadAudit.List(ports.AuditQuery{Verdict: ports.AuditDelivered})
		if len(entries) != 2 {
			t.Fatalf("delivered entries: %d", len(entries))
		}
		for _, e := range entries {
			if len(e.Deliveries) != 1 || e.Deliveries[0].Status != "sent" || len(e.Deliveries[0].Attempts) != 1 || e.Payload == "" {
				t.Errorf("entry %+v", e)
			}
		}
	})
}
