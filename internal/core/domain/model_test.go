package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLookup(t *testing.T) {
	data := map[string]any{
		"series":   map[string]any{"title": "Andor"},
		"episodes": []any{map[string]any{"n": json.Number("3")}},
		"labels":   map[string]string{"host": "nas"},
		"nothing":  nil,
	}
	cases := []struct {
		path string
		want any
		ok   bool
	}{
		{"series.title", "Andor", true},
		{"episodes.0.n", json.Number("3"), true},
		{"labels.host", "nas", true},
		{"nothing", nil, true},
		{"series.missing", nil, false},
		{"labels.missing", nil, false},
		{"episodes.1", nil, false},
		{"episodes.-1", nil, false},
		{"episodes.x", nil, false},
		{"series.title.deeper", nil, false},
		{"missing", nil, false},
	}
	for _, c := range cases {
		got, ok := Lookup(data, c.path)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got %v %v, want %v %v", c.path, got, ok, c.want, c.ok)
		}
	}
	if _, ok := Lookup("plain string", "x"); ok {
		t.Error("lookup into a scalar must fail")
	}
}

func TestDestinationEndpoint(t *testing.T) {
	d := Destination{Target: "matrix", Account: "alerts"}
	if d.Endpoint() != "matrix/alerts" || EndpointID("hook", "default") != "hook/default" {
		t.Error(d.Endpoint())
	}
}

func TestFindHeartbeat(t *testing.T) {
	specs := []Heartbeat{
		{Name: "wd", Source: "am", Labels: map[string]string{"alertname": "Watchdog"}},
		{Name: "backup", Source: "backup"},
	}
	if h, ok := FindHeartbeat(specs, &Event{Source: "backup"}); !ok || h.Name != "backup" {
		t.Errorf("label-less heartbeat: %v %v", h, ok)
	}
	if h, ok := FindHeartbeat(specs, &Event{Source: "am", Labels: map[string]string{"alertname": "Watchdog"}}); !ok || h.Name != "wd" {
		t.Errorf("labelled heartbeat: %v %v", h, ok)
	}
	if _, ok := FindHeartbeat(specs, &Event{Source: "am"}); ok {
		t.Error("missing label must not match")
	}
	if _, ok := FindHeartbeat(nil, &Event{Source: "am"}); ok {
		t.Error("no specs")
	}
}

func TestTruncate(t *testing.T) {
	if Truncate("short", 10) != "short" {
		t.Error("short strings stay")
	}
	got := Truncate(strings.Repeat("ä", 10), 5) // ä is 2 bytes, 5 cuts inside a rune
	if !utf8.ValidString(got) || !strings.HasPrefix(got, "ää") || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("got %q", got)
	}
}

func TestRendererTemplatesAndHelpers(t *testing.T) {
	r, err := NewRenderer(map[string]string{
		"helpers": `{{ upper .Source }} {{ lower "AbC" }} {{ default "none" (get .Data "missing") }} {{ default "x" (get .Data "a") }} {{ join "," .Data.list }} {{ json .Data.obj }}`,
		"generic": `overridden {{ .Source }}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := &Event{Source: "src", Kind: "nope", Data: map[string]any{"a": "A", "list": []string{"x", "y"}, "obj": map[string]any{"k": 1}}}
	out, err := r.Render("helpers", ev)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "SRC abc none A x,y {") || !strings.Contains(out, `"k": 1`) {
		t.Errorf("helpers: %q", out)
	}
	if out, _ := r.Render("", ev); out != "overridden src" {
		t.Errorf("a custom template overrides a builtin of the same name: %q", out)
	}
	if !r.Has("alertmanager") || r.Has("nope") {
		t.Error("Has")
	}
	src := r.Sources()
	if !src["alertmanager"].Builtin || src["helpers"].Builtin || src["generic"].Builtin {
		t.Errorf("origin flags: %+v", src)
	}
	if _, err := r.Render("missing", ev); err == nil {
		t.Error("unknown template must fail")
	}
	if _, err := NewRenderer(map[string]string{"x": "{{ .Unclosed"}); err == nil {
		t.Error("bad template must fail at load")
	}
}

func TestRendererTextHelpers(t *testing.T) {
	r, err := NewRenderer(map[string]string{
		"first":   `{{ get .Data "message" | firstLine }}`,
		"lines":   `{{ range lines (get .Data "message") }}[{{ trim . }}]{{ end }}`,
		"trunc":   `{{ get .Data "title" | trunc 10 }}|{{ get .Data "short" | trunc 10 }}|{{ get .Data "umlaut" | trunc 4 }}`,
		"replace": `{{ get .Data "title" | replace "Deleting item from queue with reason: " "" }}`,
		"regex":   `{{ get .Data "title" | regexFind "reason: (\\w+)" }}|{{ get .Data "title" | regexFind "queue" }}|{{ get .Data "title" | regexFind "nomatch" }}|{{ get .Data "missing" | firstLine }}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := &Event{Data: map[string]any{
		"message": "Some.Release.1080p  \r\n\nReason: Stalled\nHash: abc",
		"title":   "Deleting item from queue with reason: Stalled",
		"short":   "short",
		"umlaut":  "Grüße aus Köln",
	}}
	for name, want := range map[string]string{
		"first":   "Some.Release.1080p",
		"lines":   "[Some.Release.1080p][][Reason: Stalled][Hash: abc]",
		"trunc":   "Deleting…|short|Grü…",
		"replace": "Stalled",
		"regex":   "Stalled|queue||",
	} {
		if out, err := r.Render(name, ev); err != nil || out != want {
			t.Errorf("%s: %q, %v; want %q", name, out, err, want)
		}
	}
	for _, src := range []string{
		`{{ get .Data "title" | regexFind "(" }}`,
		`{{ if .Source }}{{ with get .Data "x" }}{{ regexFind "[" . }}{{ end }}{{ end }}`,
		`{{ range lines .Source }}{{ else }}{{ (regexFind "*" .Source) | trim }}{{ end }}`,
	} {
		if _, err := NewRenderer(map[string]string{"bad": src}); err == nil || !strings.Contains(err.Error(), "regexFind") {
			t.Errorf("an invalid literal pattern fails at load: %s: %v", src, err)
		}
	}
	if _, err := r.Render("regex", &Event{Data: "not a map"}); err != nil {
		t.Errorf("a value that is no string is rendered, not failed: %v", err)
	}
}

func TestRenderPicksTemplateByKind(t *testing.T) {
	r, _ := NewRenderer(map[string]string{"grafana": "grafana {{ .Status }}"})
	if out, _ := r.Render("", &Event{Kind: "grafana", Status: "alerting"}); out != "grafana alerting" {
		t.Errorf("kind template: %q", out)
	}
	if out, _ := r.Render("", &Event{Kind: "other", Source: "s", Data: map[string]any{"text": "hi"}}); out != "**s**\nhi" {
		t.Errorf("generic fallback: %q", out)
	}
}

func TestRouterMatchersInDetail(t *testing.T) {
	rules := []Rule{
		{Name: "not-test", Match: Match{Source: "am", Labels: map[string]string{"env": "!test|dev"}}, To: []string{"a"}},
		{Name: "resolved", Match: Match{Status: "resolved"}, To: []string{"a"}},
		{Name: "no-owner", Match: Match{Fields: map[string]string{"owner": "!.+"}}, To: []string{"a"}},
		{Name: "expr-error", Expr: "error", To: []string{"a"}},
		{Name: "end", To: []string{"b"}},
	}
	r := testRouter(t, rules...)
	explain := func(ev *Event) []TraceStep { res, _ := r.Route(ev); return res.Trace }

	tr := explain(&Event{Source: "am", Labels: map[string]string{"env": "prod"}, Status: "firing", Data: map[string]any{"owner": "me"}})
	if !tr[0].Matched {
		t.Errorf("negated label prod: %+v", tr[0])
	}
	tr = explain(&Event{Source: "am", Labels: map[string]string{"env": "test"}, Status: "resolved", Data: map[string]any{}})
	if tr[0].Matched || tr[0].Reason != `label env="test" does not match "!test|dev"` || !tr[1].Matched {
		t.Errorf("negation and status: %+v", tr[:2])
	}
	tr = explain(&Event{Source: "x", Status: "firing", Data: map[string]any{"owner": "me"}})
	if tr[0].Reason != `source "x" does not match "am"` || tr[1].Reason != `status "firing" does not match "resolved"` ||
		tr[2].Reason != `field owner="me" does not match "!.+"` || !strings.HasPrefix(tr[3].Reason, "expr error") || !tr[4].Matched {
		t.Errorf("reasons: %+v", tr)
	}
	tr = explain(&Event{Source: "x", Data: map[string]any{}})
	if !tr[2].Matched {
		t.Errorf("a negated field matches when it is missing: %+v", tr[2])
	}
	tr = explain(&Event{Source: "am", Data: map[string]any{}})
	if !tr[0].Matched {
		t.Errorf("a negated label matches when the label is missing, like negated fields: %+v", tr[0])
	}
	r2 := testRouter(t, Rule{Name: "sev", Match: Match{Labels: map[string]string{"severity": "critical"}}, To: []string{"a"}})
	if res, _ := r2.Route(&Event{}); res.Trace[0].Reason != "label severity is missing" {
		t.Errorf("missing label reason: %+v", res.Trace[0])
	}
}

func TestRouterMissingFieldReasonAndStatus(t *testing.T) {
	r := testRouter(t, Rule{Name: "f", Match: Match{Fields: map[string]string{"type": "Warning"}, Status: "firing"}, To: []string{"a"}})
	res, _ := r.Route(&Event{Status: "firing", Data: map[string]any{}})
	if res.Trace[0].Reason != "field type is missing" {
		t.Errorf("%+v", res.Trace[0])
	}
	res, _ = r.Route(&Event{Data: map[string]any{"type": "Warning"}})
	if res.Trace[0].Reason != `status "" does not match "firing"` {
		t.Errorf("empty status: %+v", res.Trace[0])
	}
}

func TestRouterBadGlobAndFieldRegex(t *testing.T) {
	rend, _ := NewRenderer(nil)
	for name, r := range map[string]Rule{
		"glob":   {Name: "g", To: []string{"a"}, Match: Match{Source: "["}},
		"status": {Name: "s", To: []string{"a"}, Match: Match{Status: "("}},
		"field":  {Name: "f", To: []string{"a"}, Match: Match{Fields: map[string]string{"x": "("}}},
		"expr":   {Name: "c", To: []string{"a"}, Expr: "1 + 1"}, // not bool
	} {
		if _, err := NewRouter([]Rule{r}, testDests, rend, testEngines(t)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRouterDeliveryBuilder(t *testing.T) {
	r := testRouter(t)
	d := r.Delivery("hb", "b", Message{Markdown: "m"})
	if d.Route != "hb" || d.Destination != "b" || d.Endpoint != "m/media" || d.Address != "!b:x" || d.Message.Markdown != "m" {
		t.Errorf("%+v", d)
	}
}
