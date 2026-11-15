//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/johnnycube/stellwerk/internal/bootstrap"
	"github.com/johnnycube/stellwerk/internal/config"
	stsetup "github.com/johnnycube/stellwerk/internal/setup"
)

// TestChatCommandsCanaryAndHealth: two bots share a room. The alerts bot
// takes commands, the media bot proves that the canary can be decrypted by
// someone else, and a destination in a room the bot is not in shows up as
// a problem.
func TestChatCommandsCanaryAndHealth(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := suffix()
	me := e.register(t, "human-"+s, "pw-human")
	alerts := e.register(t, "alerts-"+s, "pw-alerts")
	media := e.register(t, "media-"+s, "pw-media")
	h := newHuman(t, ctx, e.hs, me, "pw-human")
	room := h.room(t, ctx, "ops", true, alerts, media)
	ghost := h.room(t, ctx, "ghost", true) // nobody invited

	dir := t.TempDir()
	base := fmt.Sprintf(`
node_id: e2e
sources:
  sonarr: { kind: generic, token: so }
targets:
  matrix:
    type: matrix
    settings: { homeserver: %q, database: %q, allowed_inviters: [%q] }
    accounts:
      alerts: { user_id: %q, password: pw-alerts, pickle_key: k1, commands: true }
      media:  { user_id: %q, password: pw-media,  pickle_key: k2 }
destinations:
  ops: { target: matrix, account: alerts, address: %q }
  %%GHOST%%
routes:
  - { name: media, match: { source: sonarr }, to: ops }
canary: { to: ops, verify_with: matrix/media, every: 24h, timeout: 30s }
ui: { listen: "" }
`, e.hs, filepath.Join(dir, "{account}.db"), me, alerts, media, room)
	cfg, err := config.Parse([]byte(strings.Replace(base, "%GHOST%", "", 1)))
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
	for !app.Ready() {
		time.Sleep(100 * time.Millisecond)
	}
	waitJoined(t, ctx, h.cli, room, alerts)
	waitJoined(t, ctx, h.cli, room, media)
	time.Sleep(2 * time.Second) // let device lists settle before the first encrypted message

	t.Run("mute from the chat", func(t *testing.T) {
		if _, err := h.cli.SendMessageEvent(ctx, room, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "!sw mute 1h source=sonarr -- e2e " + s}); err != nil {
			t.Fatal(err)
		}
		r := h.waitFor(t, "command reply", func(r received) bool {
			return r.room == room && r.sender == alerts && strings.Contains(r.body, "Muted")
		})
		if !r.encrypted || r.msgType != event.MsgNotice {
			t.Errorf("%+v", r)
		}
		act := app.Silences.Active()
		if len(act) != 1 || act[0].CreatedBy != me.String() || !strings.Contains(act[0].Comment, s) {
			t.Fatalf("%+v", act)
		}
		h.mu.Lock()
		replies := 0
		for _, g := range h.got {
			if g.room == room && strings.Contains(g.body, "Muted") {
				replies++
			}
		}
		h.mu.Unlock()
		if replies != 1 {
			t.Errorf("only the account with commands: true answers, got %d replies", replies)
		}
	})

	t.Run("canary is read back by the second bot", func(t *testing.T) {
		res := app.Canary.Run(ctx)
		if !res.OK {
			t.Fatalf("%+v", res)
		}
		h.waitFor(t, "canary", func(r received) bool { return r.room == room && strings.Contains(r.body, res.Marker) })
	})

	t.Run("a room the bot is not in is reported", func(t *testing.T) {
		next, err := config.Parse([]byte(strings.Replace(base, "%GHOST%", fmt.Sprintf("ghost: { target: matrix, account: alerts, address: %q }", ghost), 1)))
		if err != nil {
			t.Fatal(err)
		}
		if err := app.Reload(next); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(app.HTTP.UIHandler())
		defer srv.Close()
		deadline := time.Now().Add(40 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get(srv.URL + "/api/flows")
			if err != nil {
				t.Fatal(err)
			}
			var flows struct {
				Targets []struct {
					Account  string   `json:"account"`
					Problems []string `json:"problems"`
				} `json:"targets"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&flows)
			resp.Body.Close()
			for _, tv := range flows.Targets {
				for _, p := range tv.Problems {
					if tv.Account == "alerts" && strings.Contains(p, ghost.String()) && strings.Contains(p, "not a member") {
						return
					}
				}
			}
			time.Sleep(time.Second)
		}
		t.Fatal("the ghost room was not reported")
	})
}

// TestInit runs "stellwerk init" against the homeserver and starts
// stellwerk from what it wrote.
func TestInit(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := suffix()
	me := e.register(t, "human-"+s, "pw-human")
	bot := e.register(t, "bot-"+s, "pw-bot")
	h := newHuman(t, ctx, e.hs, me, "pw-human")
	dir := t.TempDir()
	var out strings.Builder
	err := stsetup.Run(ctx, stsetup.Options{Homeserver: e.hs, UserID: bot.String(), Password: "pw-bot", Invite: []string{me.String()}, RoomName: "init " + s, Dir: dir},
		&out, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "warning") {
		t.Errorf("%s", out.String())
	}
	if err := stsetup.Run(ctx, stsetup.Options{Homeserver: e.hs, UserID: bot.String(), Password: "pw-bot", Invite: []string{me.String()}, Dir: dir}, io.Discard, nil); err == nil {
		t.Error("existing files must not be overwritten without -force")
	}
	if fi, _ := os.Stat(filepath.Join(dir, stsetup.EnvFile)); fi.Mode().Perm() != 0o600 {
		t.Error("env file must be private")
	}
	envFile, _ := os.ReadFile(filepath.Join(dir, stsetup.EnvFile))
	vars := map[string]string{}
	for _, line := range strings.Split(string(envFile), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(k, "#") {
			vars[k] = strings.Trim(v, "'")
		}
	}
	if vars["MATRIX_RECOVERY_KEY"] == "" || len(vars["TOKEN_ALERTMANAGER"]) < 20 {
		t.Fatalf("env: %v", vars)
	}
	var roomID id.RoomID
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "room" {
			roomID = id.RoomID(f[1])
		}
	}
	if _, err := h.cli.JoinRoomByID(ctx, roomID); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "welcome message", func(r received) bool {
		return r.room == roomID && r.encrypted && strings.Contains(r.body, "stellwerk is set up")
	})
	raw, _ := os.ReadFile(filepath.Join(dir, stsetup.ConfigFile))
	cfg, err := config.Parse([]byte(os.Expand(string(raw), func(k string) string { return vars[k] })))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrap.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
}
