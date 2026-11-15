// Package metrics exposes Prometheus metrics in the text exposition format.
//
// It is a driven adapter (a ports.Recorder that counts events and
// deliveries) plus a small HTTP handler. Gauges such as queue length or
// heartbeat state are read at scrape time from a function the composition
// root provides, so this package does not depend on the use cases.
//
// No client library: the format is simple and stellwerk only needs counters
// and gauges.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// Gauge is one sample read at scrape time.
type Gauge struct {
	Name   string
	Help   string
	Labels map[string]string
	Value  float64
}

type counter struct {
	help   string
	values map[string]float64 // rendered label set -> value
}

type Metrics struct {
	mu       sync.Mutex
	counters map[string]*counter
	Gauges   func() []Gauge
}

var _ ports.Recorder = (*Metrics)(nil)

func New() *Metrics {
	m := &Metrics{counters: map[string]*counter{}}
	m.define("stellwerk_events_total", "Events received, by source and verdict.")
	m.define("stellwerk_route_matches_total", "Events a rule matched (delivered or dropped).")
	m.define("stellwerk_deliveries_total", "Finished deliveries, by route, destination and result.")
	m.define("stellwerk_rate_limited_total", "Requests refused with 429, by source.")
	return m
}

func (m *Metrics) define(name, help string) {
	m.counters[name] = &counter{help: help, values: map[string]float64{}}
}

func (m *Metrics) inc(name string, labels map[string]string) {
	m.mu.Lock()
	m.counters[name].values[renderLabels(labels)]++
	m.mu.Unlock()
}

func (m *Metrics) RecordEvent(e ports.EventRecord) {
	m.inc("stellwerk_events_total", map[string]string{"source": e.Source, "verdict": e.Verdict})
	for _, r := range e.Routes {
		m.inc("stellwerk_route_matches_total", map[string]string{"route": r})
	}
}

func (m *Metrics) RecordDelivery(d domain.Delivery, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.inc("stellwerk_deliveries_total", map[string]string{"route": d.Route, "destination": d.Destination, "result": result})
}

// RateLimited counts a refused request.
func (m *Metrics) RateLimited(source string) {
	m.inc("stellwerk_rate_limited_total", map[string]string{"source": source})
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.Write(w)
	})
}

// Write renders all metrics, sorted, so the output is stable.
func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	names := make([]string, 0, len(m.counters))
	for n := range m.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := m.counters[n]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", n, c.help, n)
		keys := make([]string, 0, len(c.values))
		for k := range c.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s%s %s\n", n, k, formatValue(c.values[k]))
		}
	}
	m.mu.Unlock()

	if m.Gauges == nil {
		return
	}
	byName := map[string][]Gauge{}
	var order []string
	for _, g := range m.Gauges() {
		if _, seen := byName[g.Name]; !seen {
			order = append(order, g.Name)
		}
		byName[g.Name] = append(byName[g.Name], g)
	}
	sort.Strings(order)
	for _, n := range order {
		gs := byName[n]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", n, gs[0].Help, n)
		sort.Slice(gs, func(i, j int) bool { return renderLabels(gs[i].Labels) < renderLabels(gs[j].Labels) })
		for _, g := range gs {
			fmt.Fprintf(w, "%s%s %s\n", n, renderLabels(g.Labels), formatValue(g.Value))
		}
	}
}

func renderLabels(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + `="` + escape(l[k]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func formatValue(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// Bool is a helper for 0/1 gauges.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
