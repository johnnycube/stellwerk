package target

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"

	"go.yaml.in/yaml/v4"
)

func node(t *testing.T, s string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return n.Content[0]
}

func TestMergeSettings(t *testing.T) {
	m, err := MergeSettings(node(t, "homeserver: hs\nuser_id: shared\ninviters: [a]"), node(t, "user_id: own\npassword: pw"))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := m.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["homeserver"] != "hs" || got["user_id"] != "own" || got["password"] != "pw" || len(got["inviters"].([]any)) != 1 {
		t.Errorf("merged: %v", got)
	}
	if _, err := MergeSettings(node(t, "[1]"), nil); err == nil {
		t.Error("expected error for non-mapping")
	}
	if m, _ := MergeSettings(nil, nil); len(m.Content) != 0 {
		t.Error("empty merge")
	}
}

func TestEnvExpand(t *testing.T) {
	e := Env{Target: "matrix", Account: "alerts", NodeID: "a"}
	if got := e.Expand("/data/{target}-{account}-{node}.db"); got != "/data/matrix-alerts-a.db" {
		t.Error(got)
	}
}

func TestTitle(t *testing.T) {
	for in, want := range map[string]string{
		"🔥 **Disk full** FIRING · critical\n- x": "🔥 Disk full FIRING · critical",
		"\n\n# Title `code`":                     "Title code",
		"":                                       "message",
		strings.Repeat("a", 200):                 strings.Repeat("a", 119) + "…",
	} {
		if got := Title(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestStatusError(t *testing.T) {
	resp := func(code int, hdr ...string) *http.Response {
		r := &http.Response{StatusCode: code, Status: http.StatusText(code), Header: http.Header{}, Body: io.NopCloser(strings.NewReader(" nope "))}
		if len(hdr) == 2 {
			r.Header.Set(hdr[0], hdr[1])
		}
		return r
	}
	if err := StatusError("hook", resp(404)); !ports.IsPermanent(err) || err.Error() != "hook answered Not Found: nope" {
		t.Errorf("404: %v", err)
	}
	for _, c := range []int{408, 425, 500, 503} {
		if err := StatusError("hook", resp(c)); ports.IsPermanent(err) {
			t.Errorf("%d must be retried", c)
		}
	}
	if d, ok := ports.RetryAfterOf(StatusError("hook", resp(429, "Retry-After", "7"))); !ok || d != 7*time.Second {
		t.Errorf("429: %v %v", d, ok)
	}
	if err := StatusError("hook", resp(302)); err == nil || ports.IsPermanent(err) {
		t.Error("3xx is an error, not permanent")
	}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	if d := ParseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); d != 90*time.Second {
		t.Errorf("date: %v", d)
	}
	for _, v := range []string{"", "-1", "soon", now.Add(-time.Minute).Format(http.TimeFormat)} {
		if ParseRetryAfter(v, now) != 0 {
			t.Errorf("%q", v)
		}
	}
}
