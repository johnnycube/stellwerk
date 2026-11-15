package bootstrap

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
	"github.com/johnnycube/stellwerk/internal/config"
	"github.com/johnnycube/stellwerk/internal/core/usecase"
)

// RunTests runs the tests: section of the config against the rules, with
// the real decoders and router and nothing sent. It prints one line per
// test and returns how many failed.
func (a *App) RunTests(w io.Writer) int {
	tests := a.Cfg.Tests
	if len(tests) == 0 {
		fmt.Fprintln(w, "no tests: add a tests: section to the config")
		return 0
	}
	sim := usecase.NewSimulateEvents(a.Router, a.Spec.Heartbeats)
	failed := 0
	for _, tc := range tests {
		if problems := a.runTest(sim, tc); len(problems) > 0 {
			failed++
			fmt.Fprintf(w, "FAIL  %s\n", tc.Name)
			for _, p := range problems {
				fmt.Fprintf(w, "      %s\n", p)
			}
		} else {
			fmt.Fprintf(w, "ok    %s\n", tc.Name)
		}
	}
	fmt.Fprintf(w, "\n%d of %d tests passed\n", len(tests)-failed, len(tests))
	return failed
}

func (a *App) runTest(sim *usecase.SimulateEvents, tc config.Test) []string {
	src, _, ok := a.Sources.Resolve(tc.Source)
	if !ok {
		return []string{fmt.Sprintf("no source %q and no \"*\" fallback", tc.Source)}
	}
	q, err := url.ParseQuery(strings.TrimPrefix(tc.Query, "?"))
	if err != nil {
		return []string{"query: " + err.Error()}
	}
	h := http.Header{}
	for k, v := range tc.Header {
		h.Set(k, v)
	}
	events, err := src.Decoder.Decode(source.Input{Source: tc.Source, Body: []byte(tc.Body), Query: q, Header: h})
	if err != nil {
		return []string{"the decoder refuses the body: " + err.Error()}
	}
	var routes, to, msgs []string
	var dropped, heartbeat string
	var loud []bool
	var prios []string
	for _, s := range sim.Simulate(events) {
		if s.Err != "" {
			return []string{"routing error: " + s.Err}
		}
		if s.Heartbeat != "" {
			heartbeat = s.Heartbeat
		}
		if s.DroppedBy != "" {
			dropped = s.DroppedBy
		}
		for _, d := range s.Deliveries {
			if len(routes) == 0 || routes[len(routes)-1] != d.Route {
				routes = append(routes, d.Route)
			}
			to = append(to, d.Destination)
			msgs = append(msgs, d.Message.Markdown)
			loud = append(loud, d.Message.Loud)
			prios = append(prios, d.Message.Priority)
		}
	}
	e := tc.Expect
	var p []string
	if e.Routes != nil && !slices.Equal(routes, e.Routes) {
		p = append(p, fmt.Sprintf("routes: got %v, want %v", routes, e.Routes))
	}
	if e.To != nil && !slices.Equal(to, e.To) {
		p = append(p, fmt.Sprintf("to: got %v, want %v", to, e.To))
	}
	if e.DroppedBy != "" && dropped != e.DroppedBy {
		p = append(p, fmt.Sprintf("dropped_by: got %q, want %q", dropped, e.DroppedBy))
	}
	if e.Unmatched && (len(to) > 0 || dropped != "" || heartbeat != "") {
		p = append(p, fmt.Sprintf("unmatched: but it went to %v (dropped by %q, heartbeat %q)", to, dropped, heartbeat))
	}
	if e.Heartbeat != "" && heartbeat != e.Heartbeat {
		p = append(p, fmt.Sprintf("heartbeat: got %q, want %q", heartbeat, e.Heartbeat))
	}
	for _, want := range e.Contains {
		if len(msgs) == 0 {
			p = append(p, fmt.Sprintf("contains %q: nothing was sent", want))
		}
		for i, m := range msgs {
			if !strings.Contains(m, want) {
				p = append(p, fmt.Sprintf("contains %q: the message to %s is:\n%s", want, to[i], indent(m)))
			}
		}
	}
	for i := range to {
		if e.Loud != nil && loud[i] != *e.Loud {
			p = append(p, fmt.Sprintf("loud: the message to %s is loud=%v", to[i], loud[i]))
		}
		if e.Priority != "" && prios[i] != e.Priority {
			p = append(p, fmt.Sprintf("priority: the message to %s has %q, want %q", to[i], prios[i], e.Priority))
		}
	}
	return p
}

func indent(s string) string {
	return "        " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n        ")
}
