package domain

import (
	"testing"
	"time"
)

func TestParseMatchers(t *testing.T) {
	m, err := ParseMatchers("source=k8s* status=firing severity=warning|info ns=!prod")
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "k8s*" || m.Status != "firing" || m.Labels["severity"] != "warning|info" || m.Labels["ns"] != "!prod" {
		t.Errorf("%+v", m)
	}
	if got := FormatMatchers(m); got != "source=k8s* status=firing ns=!prod severity=warning|info" {
		t.Errorf("format: %q", got)
	}
	for _, bad := range []string{"", "   ", "source", "=x", "x=", "severity=(", "source=["} {
		if _, err := ParseMatchers(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	c, _ := CompileMatch(m)
	if ok, _ := c.Explain(&Event{Source: "k8s-events", Status: "firing", Labels: map[string]string{"severity": "info", "ns": "media"}}); !ok {
		t.Error("should match")
	}
	if ok, why := c.Explain(&Event{Source: "k8s-events", Status: "firing", Labels: map[string]string{"severity": "info", "ns": "prod"}}); ok || why == "" {
		t.Error("ns=prod excluded")
	}
}

func TestSilenceActive(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	s := Silence{Starts: now, Ends: now.Add(time.Hour), Match: Match{Source: "k8s"}}
	if !s.Active(now) || !s.Active(now.Add(59*time.Minute)) || s.Active(now.Add(time.Hour)) || s.Active(now.Add(-time.Second)) {
		t.Error("active window")
	}
	if s.Describe() != "source=k8s" {
		t.Error(s.Describe())
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "2h": 2 * time.Hour, "1d": 24 * time.Hour, "1d12h": 36 * time.Hour, " 30s ": 30 * time.Second} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0s", "-1h", "xd", "1.5d", "soon", "-1d"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
}
