package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonData(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

var testDests = map[string]Destination{
	"a": {Alias: "a", Target: "m", Account: "alerts", Address: "!a:x"},
	"b": {Alias: "b", Target: "m", Account: "media", Address: "!b:x"},
	"p": {Alias: "p", Target: "hook", Account: "default"},
}

func testRouter(t *testing.T, rules ...Rule) *Router {
	t.Helper()
	rend, err := NewRenderer(nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRouter(rules, testDests, rend, testEngines(t))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var all = Rule{Name: "all", To: []string{"a"}}

func TestRouteOrderDropContinueMultiTarget(t *testing.T) {
	r := testRouter(t,
		Rule{Name: "noise", Match: Match{Source: "k8s", Fields: map[string]string{"type": "Normal"}}, Drop: true},
		Rule{Name: "both", Match: Match{Source: "k8s"}, To: []string{"a", "p"}, Loud: true, Continue: true},
		Rule{Name: "also", Match: Match{Source: "k*"}, To: []string{"b"}},
		Rule{Name: "never", To: []string{"a"}},
	)
	res, _ := r.Route(&Event{Source: "k8s", Kind: "generic", Data: jsonData(t, `{"type":"Warning","message":"m"}`)})
	if len(res.Deliveries) != 3 {
		t.Fatalf("want 3 deliveries, got %+v", res.Deliveries)
	}
	if d := res.Deliveries[1]; d.Destination != "p" || d.Endpoint != "hook/default" || !d.Message.Loud || d.Message.Route != "both" {
		t.Errorf("multi-target delivery: %+v", d)
	}
	if d := res.Deliveries[2]; d.Endpoint != "m/media" || d.Address != "!b:x" {
		t.Errorf("multi-target delivery: %+v", d)
	}
	if res.Trace[3].Action != "skipped" || res.Trace[0].Reason != `field type="Warning" does not match "Normal"` {
		t.Errorf("trace: %+v", res.Trace)
	}
	res, _ = r.Route(&Event{Source: "k8s", Data: jsonData(t, `{"type":"Normal"}`)})
	if res.DroppedBy != "noise" || len(res.Deliveries) != 0 {
		t.Errorf("drop: %+v", res)
	}
}

func TestBuiltinAlertmanagerTemplate(t *testing.T) {
	r := testRouter(t, all)
	data := jsonData(t, `{"status":"firing","alerts":[
	  {"status":"firing","labels":{"alertname":"Disk"},"annotations":{"summary":"Disk full","description":"91%"},"generatorURL":"http://p"},
	  {"status":"resolved","labels":{"alertname":"Disk"},"annotations":{"summary":"Old one"}}]}`)
	res, err := r.Route(&Event{Source: "am", Kind: "alertmanager", Status: "firing", Labels: map[string]string{"alertname": "Disk", "severity": "critical"}, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	md := res.Deliveries[0].Message.Markdown
	for _, want := range []string{"🔥 **Disk** FIRING · critical", "- Disk full", "91%", "[source](http://p)", "~~Old one~~"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
}

func TestGenericTemplate(t *testing.T) {
	r := testRouter(t, all)
	res, _ := r.Route(&Event{Source: "backup", Kind: "generic", Data: jsonData(t, `{"title":"Done","message":"42 GB"}`)})
	if got := res.Deliveries[0].Message.Markdown; got != "**backup**: Done\n42 GB" {
		t.Errorf("got %q", got)
	}
	res, _ = r.Route(&Event{Source: "x", Kind: "generic", Data: jsonData(t, `{"a":[1]}`)})
	if !strings.Contains(res.Deliveries[0].Message.Markdown, "```json") {
		t.Errorf("json fallback missing")
	}
}

func TestRouterRejectsBadRules(t *testing.T) {
	rend, _ := NewRenderer(nil)
	for name, r := range map[string]Rule{
		"unknown template":    {Name: "r", To: []string{"a"}, Template: "missing"},
		"unknown destination": {Name: "r", To: []string{"nope"}},
		"bad regex":           {Name: "r", To: []string{"a"}, Match: Match{Labels: map[string]string{"x": "("}}},
		"bad expr":            {Name: "r", To: []string{"a"}, Expr: "source =="},
		"unknown engine":      {Name: "r", To: []string{"a"}, Expr: "true", Engine: "nope"},
	} {
		if _, err := NewRouter([]Rule{r}, testDests, rend, testEngines(t)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestHeartbeatMatches(t *testing.T) {
	h := Heartbeat{Source: "am", Labels: map[string]string{"alertname": "Watchdog"}}
	if !h.Matches(&Event{Source: "am", Labels: map[string]string{"alertname": "Watchdog", "x": "y"}}) ||
		h.Matches(&Event{Source: "am", Labels: map[string]string{"alertname": "Other"}}) ||
		h.Matches(&Event{Source: "other", Labels: map[string]string{"alertname": "Watchdog"}}) {
		t.Error("heartbeat matching")
	}
}
