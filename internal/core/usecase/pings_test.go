package usecase

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

func pingWorld(t *testing.T) *world {
	return newWorld(t, func(s *domain.Spec) {
		s.Heartbeats = append(s.Heartbeats, domain.Heartbeat{Name: "backup", Timeout: 25 * time.Hour, To: []string{"critical"}})
	})
}

func TestPing_KeepsHeartbeatAlive(t *testing.T) {
	w := pingWorld(t)
	w.coord.SetLeader(true)
	w.clock.Advance(24 * time.Hour)
	if err := w.hb.Ping("backup"); err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(24 * time.Hour)
	for _, c := range w.hb.Check() {
		if c.Name == "backup" {
			t.Fatal("pinged within the timeout, must not be missing")
		}
	}
	w.clock.Advance(2 * time.Hour)
	w.hb.Check()
	eventually(t, "missing alarm", func() bool {
		for _, s := range w.alerts.Sent() {
			if strings.Contains(s.Msg.Markdown, "backup did not check in") {
				return true
			}
		}
		return false
	})
	if err := w.hb.Ping("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
}

func TestPing_FailAlertsAtOnceAndRecovers(t *testing.T) {
	w := pingWorld(t) // not the leader: the node that gets the request answers it
	if err := w.hb.Fail("backup", "restic: repository locked\n"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "failure alert", func() bool { return len(w.alerts.Sent()) == 1 })
	m := w.alerts.Sent()[0].Msg
	if !strings.Contains(m.Markdown, "❌ **backup failed**") || !strings.Contains(m.Markdown, "repository locked") || m.Priority != domain.PriorityUrgent || !m.Loud {
		t.Errorf("%+v", m)
	}
	if st := states(w)["backup"]; !st.Failed || st.Missing {
		t.Errorf("%+v", st)
	}
	w.hb.Check()
	time.Sleep(10 * time.Millisecond)
	if len(w.alerts.Sent()) != 1 {
		t.Error("a failure is a sign of life, no missing alarm on top")
	}
	_ = w.hb.Ping("backup")
	eventually(t, "recovery", func() bool { return len(w.alerts.Sent()) == 2 })
	if m := w.alerts.Sent()[1].Msg; !strings.Contains(m.Markdown, "backup succeeded") || m.Loud {
		t.Errorf("%+v", m)
	}
	_ = w.hb.Ping("backup")
	time.Sleep(10 * time.Millisecond)
	if len(w.alerts.Sent()) != 2 {
		t.Error("only the first success after a failure is announced")
	}
	if err := w.hb.Fail("nope", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("%v", err)
	}
	if e := w.last(); e.Kind != "heartbeat-alert" || e.Status != "succeeded" {
		t.Errorf("audit %+v", e)
	}
}

func states(w *world) map[string]ports.HeartbeatState {
	out := map[string]ports.HeartbeatState{}
	for _, s := range w.hb.States() {
		out[s.Name] = s
	}
	return out
}
