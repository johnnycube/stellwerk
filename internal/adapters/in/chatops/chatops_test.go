package chatops

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
	"github.com/johnnycube/stellwerk/internal/core/usecase"
)

type topo struct{ t ports.Topology }

func (t topo) Describe() ports.Topology { return t.t }

type canary struct {
	ok   bool
	runs int
}

func (c *canary) Run(context.Context) ports.CanaryResult {
	c.runs++
	return ports.CanaryResult{OK: c.ok, Marker: "canary-x", Problems: []string{"critical: down"}}
}
func (c *canary) Last() (ports.CanaryResult, bool) {
	return ports.CanaryResult{OK: true, Time: time.Now()}, c.runs > 0
}

func handler(t *testing.T) (*Handler, *usecase.ManageSilences) {
	s := usecase.NewManageSilences(portstest.NewSilenceStore(), portstest.NewClock(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &Handler{Silences: s, Describe: topo{ports.Topology{
		Heartbeats: []ports.HeartbeatState{{Heartbeat: domain.Heartbeat{Name: "wd"}, LastSeen: time.Now()}, {Heartbeat: domain.Heartbeat{Name: "backup"}, Failed: true}, {Heartbeat: domain.Heartbeat{Name: "cron"}, Missing: true}},
		Cluster:    ports.ClusterStatus{Enabled: true, NodeID: "a", Leader: "a"}}}}, s
}

func TestIsCommand(t *testing.T) {
	for text, want := range map[string]bool{"!stellwerk": true, "!sw status": true, " !stellwerk mute 1h x=y": true, "!swx": false, "hello": false, "!stellwerkx": false} {
		if IsCommand(text) != want {
			t.Errorf("%q", text)
		}
	}
	h, _ := handler(t)
	if _, ok := h.Handle(context.Background(), Command{Text: "hi"}); ok {
		t.Error("not a command")
	}
}

func TestMuteUnmuteList(t *testing.T) {
	h, s := handler(t)
	ctx := context.Background()
	reply, ok := h.Handle(ctx, Command{ID: "$evt1", Sender: "@me:x", Text: "!sw mute 2h source=k8s severity=warning -- cluster upgrade"})
	if !ok || !strings.Contains(reply, "🔕 Muted `source=k8s severity=warning`") {
		t.Fatalf("%q", reply)
	}
	act := s.Active()
	if len(act) != 1 || act[0].Comment != "cluster upgrade" || act[0].CreatedBy != "@me:x" {
		t.Fatalf("%+v", act)
	}
	again, _ := h.Handle(ctx, Command{ID: "$evt1", Sender: "@me:x", Text: "!sw mute 2h source=k8s severity=warning -- cluster upgrade"})
	if len(s.Active()) != 1 || !strings.Contains(again, act[0].ID) {
		t.Error("the same chat message must give the same silence (every node of a cluster gets it)")
	}
	if list, _ := h.Handle(ctx, Command{Text: "!stellwerk silences"}); !strings.Contains(list, act[0].ID) || !strings.Contains(list, "cluster upgrade") || !strings.Contains(list, "@me:x") {
		t.Errorf("%q", list)
	}
	if r, _ := h.Handle(ctx, Command{Text: "!stellwerk unmute " + act[0].ID}); !strings.Contains(r, "ended") || len(s.Active()) != 0 {
		t.Errorf("%q", r)
	}
	if r, _ := h.Handle(ctx, Command{Text: "!stellwerk silences"}); r != "Nothing is muted." {
		t.Error(r)
	}
	for _, bad := range []string{"!sw mute", "!sw mute 2h", "!sw mute soon source=x", "!sw unmute", "!sw unmute nope"} {
		if r, _ := h.Handle(ctx, Command{Text: bad}); !strings.Contains(r, "❌") && !strings.Contains(r, "Write") && !strings.Contains(r, "Which") {
			t.Errorf("%s: %q", bad, r)
		}
	}
}

func TestStatusHelpCanaryAndLeader(t *testing.T) {
	h, _ := handler(t)
	ctx := context.Background()
	st, _ := h.Handle(ctx, Command{Text: "!sw status"})
	for _, want := range []string{"leader `a`", "`backup` ❌ failed", "`cron` 🚨 missing", "`wd` ✅", "0 active silences"} {
		if !strings.Contains(st, want) {
			t.Errorf("missing %q in %q", want, st)
		}
	}
	if r, _ := h.Handle(ctx, Command{Text: "!sw canary"}); !strings.Contains(r, "No canary") {
		t.Error(r)
	}
	c := &canary{}
	h.Canary = c
	if r, _ := h.Handle(ctx, Command{Text: "!sw canary"}); !strings.Contains(r, "failed: critical: down") {
		t.Error(r)
	}
	c.ok = true
	if r, _ := h.Handle(ctx, Command{Text: "!sw canary"}); !strings.Contains(r, "arrived") {
		t.Error(r)
	}
	if st, _ := h.Handle(ctx, Command{Text: "!sw status"}); !strings.Contains(st, "last canary") {
		t.Error(st)
	}
	h.Canary = &canary{}
	if st, _ := h.Handle(ctx, Command{Text: "!sw status"}); !strings.Contains(st, "no canary sent yet") {
		t.Error(st)
	}
	if r, _ := h.Handle(ctx, Command{Text: "!stellwerk"}); !strings.Contains(r, "stellwerk commands") {
		t.Error(r)
	}
	h.Leader = func() bool { return false }
	if _, ok := h.Handle(ctx, Command{Text: "!sw status"}); ok {
		t.Error("followers do not answer")
	}
	before := c.runs
	if _, ok := h.Handle(ctx, Command{Text: "!sw canary"}); ok || c.runs != before {
		t.Error("followers do not run the canary")
	}
}
