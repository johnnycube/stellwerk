package domain

import (
	"strings"
	"testing"
)

func TestRouterAttachesFallbacks(t *testing.T) {
	dests := map[string]Destination{
		"chat": {Alias: "chat", Target: "m", Account: "a", Address: "!r", Fallback: []string{"mail", "push"}},
		"mail": {Alias: "mail", Target: "email", Account: "default", Address: "me@x"},
		"push": {Alias: "push", Target: "ntfy", Account: "default", Address: "alerts", Fallback: []string{"chat"}},
	}
	rend, _ := NewRenderer(nil)
	r, err := NewRouter([]Rule{{Name: "r", To: []string{"chat"}, Loud: true}}, dests, rend, testEngines(t))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := r.Route(&Event{Source: "s", Data: map[string]any{"text": "hi"}})
	d := res.Deliveries[0]
	if len(d.Fallback) != 2 || d.Fallback[0].Destination != "mail" || d.Fallback[0].Endpoint != "email/default" ||
		d.Fallback[0].Address != "me@x" || d.Fallback[0].FallbackFor != "chat" || d.Fallback[1].Destination != "push" {
		t.Fatalf("%+v", d.Fallback)
	}
	if len(d.Fallback[1].Fallback) != 0 {
		t.Error("fallbacks carry no fallbacks of their own")
	}
	if !strings.HasSuffix(d.Fallback[0].Message.Markdown, "\n\n↪ _Fallback: could not deliver to `chat`._") ||
		!strings.HasPrefix(d.Fallback[0].Message.Markdown, d.Message.Markdown) ||
		!strings.Contains(d.Fallback[0].Message.Markdown, "hi") || d.Message.Markdown == d.Fallback[0].Message.Markdown {
		t.Errorf("%q", d.Fallback[0].Message.Markdown)
	}
	if d.Message.Priority != PriorityHigh || d.Fallback[0].Message.Priority != PriorityHigh {
		t.Error("loud means high")
	}
}

func TestRouterRejectsBadFallbacks(t *testing.T) {
	rend, _ := NewRenderer(nil)
	for name, dests := range map[string]map[string]Destination{
		"self":    {"a": {Alias: "a", Fallback: []string{"a"}}},
		"unknown": {"a": {Alias: "a", Fallback: []string{"nope"}}},
	} {
		if _, err := NewRouter(nil, dests, rend, testEngines(t)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestPriority(t *testing.T) {
	r := testRouter(t,
		Rule{Name: "explicit", Match: Match{Source: "u"}, To: []string{"a"}, Priority: PriorityUrgent},
		Rule{Name: "mention", Match: Match{Source: "m"}, To: []string{"a"}, Mention: []string{"@me"}},
		Rule{Name: "quiet", To: []string{"a"}},
	)
	for src, want := range map[string]string{"u": PriorityUrgent, "m": PriorityHigh, "x": PriorityNormal} {
		res, _ := r.Route(&Event{Source: src})
		if got := res.Deliveries[0].Message.Priority; got != want {
			t.Errorf("%s: %s, want %s", src, got, want)
		}
	}
	if d := r.Delivery("hb", "a", Message{}); d.Message.Priority != PriorityNormal {
		t.Errorf("builder default: %q", d.Message.Priority)
	}
	rend, _ := NewRenderer(nil)
	if _, err := NewRouter([]Rule{{Name: "r", To: []string{"a"}, Priority: "panic"}}, testDests, rend, testEngines(t)); err == nil {
		t.Error("unknown priority must fail")
	}
	if !ValidPriority("") || !ValidPriority("low") || ValidPriority("LOW") {
		t.Error("ValidPriority")
	}
}

func TestHelpers(t *testing.T) {
	r := testRouter(t)
	if !r.HasDestination("a") || r.HasDestination("nope") {
		t.Error("HasDestination")
	}
	if PriorityRank(PriorityUrgent) != 3 || PriorityRank(PriorityLow) != 0 || PriorityRank("") != 1 || PriorityRank("weird") != 1 {
		t.Error("PriorityRank")
	}
}
