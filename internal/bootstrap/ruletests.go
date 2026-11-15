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
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
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
	events, problem := a.decodeTest(tc)
	if problem != "" {
		return []string{problem}
	}
	o, errMsg := collectOutcome(sim.Simulate(events))
	if errMsg != "" {
		return []string{"routing error: " + errMsg}
	}
	e := tc.Expect
	var p []string
	p = append(p, checkRoutes(e, o)...)
	p = append(p, checkTo(e, o)...)
	p = append(p, checkDroppedBy(e, o)...)
	p = append(p, checkUnmatched(e, o)...)
	p = append(p, checkHeartbeat(e, o)...)
	p = append(p, checkContains(e, o)...)
	p = append(p, checkMessages(e, o)...)
	return p
}

// decodeTest runs the test's request through its source's decoder.
func (a *App) decodeTest(tc config.Test) ([]*domain.Event, string) {
	src, _, ok := a.Sources.Resolve(tc.Source)
	if !ok {
		return nil, fmt.Sprintf("no source %q and no \"*\" fallback", tc.Source)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(tc.Query, "?"))
	if err != nil {
		return nil, "query: " + err.Error()
	}
	h := http.Header{}
	for k, v := range tc.Header {
		h.Set(k, v)
	}
	events, err := src.Decoder.Decode(source.Input{Source: tc.Source, Body: []byte(tc.Body), Query: q, Header: h})
	if err != nil {
		return nil, "the decoder refuses the body: " + err.Error()
	}
	return events, ""
}

// testOutcome is what the simulated events did, one entry per delivery.
type testOutcome struct {
	routes, to, msgs, prios []string
	loud                    []bool
	dropped, heartbeat      string
}

func collectOutcome(sims []ports.Simulation) (testOutcome, string) {
	var o testOutcome
	for _, s := range sims {
		if s.Err != "" {
			return o, s.Err
		}
		if s.Heartbeat != "" {
			o.heartbeat = s.Heartbeat
		}
		if s.DroppedBy != "" {
			o.dropped = s.DroppedBy
		}
		for _, d := range s.Deliveries {
			if len(o.routes) == 0 || o.routes[len(o.routes)-1] != d.Route {
				o.routes = append(o.routes, d.Route)
			}
			o.to = append(o.to, d.Destination)
			o.msgs = append(o.msgs, d.Message.Markdown)
			o.loud = append(o.loud, d.Message.Loud)
			o.prios = append(o.prios, d.Message.Priority)
		}
	}
	return o, ""
}

func checkRoutes(e config.TestExpect, o testOutcome) []string {
	if e.Routes != nil && !slices.Equal(o.routes, e.Routes) {
		return []string{fmt.Sprintf("routes: got %v, want %v", o.routes, e.Routes)}
	}
	return nil
}

func checkTo(e config.TestExpect, o testOutcome) []string {
	if e.To != nil && !slices.Equal(o.to, e.To) {
		return []string{fmt.Sprintf("to: got %v, want %v", o.to, e.To)}
	}
	return nil
}

func checkDroppedBy(e config.TestExpect, o testOutcome) []string {
	if e.DroppedBy != "" && o.dropped != e.DroppedBy {
		return []string{fmt.Sprintf("dropped_by: got %q, want %q", o.dropped, e.DroppedBy)}
	}
	return nil
}

func checkUnmatched(e config.TestExpect, o testOutcome) []string {
	if e.Unmatched && (len(o.to) > 0 || o.dropped != "" || o.heartbeat != "") {
		return []string{fmt.Sprintf("unmatched: but it went to %v (dropped by %q, heartbeat %q)", o.to, o.dropped, o.heartbeat)}
	}
	return nil
}

func checkHeartbeat(e config.TestExpect, o testOutcome) []string {
	if e.Heartbeat != "" && o.heartbeat != e.Heartbeat {
		return []string{fmt.Sprintf("heartbeat: got %q, want %q", o.heartbeat, e.Heartbeat)}
	}
	return nil
}

func checkContains(e config.TestExpect, o testOutcome) []string {
	var p []string
	for _, want := range e.Contains {
		if len(o.msgs) == 0 {
			p = append(p, fmt.Sprintf("contains %q: nothing was sent", want))
		}
		for i, m := range o.msgs {
			if !strings.Contains(m, want) {
				p = append(p, fmt.Sprintf("contains %q: the message to %s is:\n%s", want, o.to[i], indent(m)))
			}
		}
	}
	return p
}

// checkMessages checks loud and priority of each delivered message.
func checkMessages(e config.TestExpect, o testOutcome) []string {
	var p []string
	for i := range o.to {
		if e.Loud != nil && o.loud[i] != *e.Loud {
			p = append(p, fmt.Sprintf("loud: the message to %s is loud=%v", o.to[i], o.loud[i]))
		}
		if e.Priority != "" && o.prios[i] != e.Priority {
			p = append(p, fmt.Sprintf("priority: the message to %s has %q, want %q", o.to[i], o.prios[i], e.Priority))
		}
	}
	return p
}

func indent(s string) string {
	return "        " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n        ")
}
