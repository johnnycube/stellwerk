package domain

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

type matcher struct {
	re  *regexp.Regexp
	neg bool
}

func (m matcher) ok(v string, present bool) bool {
	hit := present && m.re.MatchString(v)
	return hit != m.neg
}

func compileMatcher(expr string) (matcher, error) {
	neg := strings.HasPrefix(expr, "!")
	re, err := regexp.Compile("^(?:" + strings.TrimPrefix(expr, "!") + ")$")
	return matcher{re: re, neg: neg}, err
}

// compiledMatch is a Match with its regexes compiled. Routes and silences
// use it.
type compiledMatch struct {
	m      Match
	status *matcher
	labels map[string]matcher
	fields map[string]matcher
}

// CompileMatch checks and compiles a Match.
func CompileMatch(m Match) (*compiledMatch, error) {
	c := &compiledMatch{m: m, labels: map[string]matcher{}, fields: map[string]matcher{}}
	if _, err := path.Match(m.Source, ""); err != nil {
		return nil, fmt.Errorf("bad source glob: %w", err)
	}
	if m.Status != "" {
		x, err := compileMatcher(m.Status)
		if err != nil {
			return nil, fmt.Errorf("status: %w", err)
		}
		c.status = &x
	}
	for k, v := range m.Labels {
		x, err := compileMatcher(v)
		if err != nil {
			return nil, fmt.Errorf("label %s: %w", k, err)
		}
		c.labels[k] = x
	}
	for k, v := range m.Fields {
		x, err := compileMatcher(v)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		c.fields[k] = x
	}
	return c, nil
}

// Explain reports whether ev matches and, if not, which condition failed.
func (c *compiledMatch) Explain(ev *Event) (bool, string) {
	if c.m.Source != "" {
		if ok, _ := path.Match(c.m.Source, ev.Source); !ok {
			return false, fmt.Sprintf("source %q does not match %q", ev.Source, c.m.Source)
		}
	}
	if c.status != nil && !c.status.ok(ev.Status, ev.Status != "") {
		return false, fmt.Sprintf("status %q does not match %q", ev.Status, c.m.Status)
	}
	for _, k := range sortedKeys(c.labels) {
		v, present := ev.Labels[k]
		if !c.labels[k].ok(v, present) {
			if !present {
				return false, fmt.Sprintf("label %s is missing", k)
			}
			return false, fmt.Sprintf("label %s=%q does not match %q", k, v, c.m.Labels[k])
		}
	}
	for _, k := range sortedKeys(c.fields) {
		v, present := Lookup(ev.Data, k)
		present = present && v != nil
		if !c.fields[k].ok(fmt.Sprint(v), present) {
			if !present {
				return false, fmt.Sprintf("field %s is missing", k)
			}
			return false, fmt.Sprintf("field %s=%q does not match %q", k, fmt.Sprint(v), c.m.Fields[k])
		}
	}
	return true, ""
}

type route struct {
	cfg   Rule
	match *compiledMatch
	expr  Predicate
}

// Router evaluates rules in order and produces deliveries. It is pure:
// same event in, same result out.
type Router struct {
	routes       []route
	destinations map[string]Destination
	renderer     *Renderer
}

// NewRouter compiles the rules. Expressions are compiled by the engine a
// rule names, or the default one of engines.
func NewRouter(rules []Rule, destinations map[string]Destination, renderer *Renderer, engines Engines) (*Router, error) {
	r := &Router{destinations: destinations, renderer: renderer}
	for alias, d := range destinations {
		for _, fb := range d.Fallback {
			if fb == alias {
				return nil, fmt.Errorf("destination %s: cannot be its own fallback", alias)
			}
			if _, ok := destinations[fb]; !ok {
				return nil, fmt.Errorf("destination %s: unknown fallback %q", alias, fb)
			}
		}
	}
	for _, rc := range rules {
		for _, alias := range rc.To {
			if _, ok := destinations[alias]; !ok {
				return nil, fmt.Errorf("rule %s: unknown destination %q", rc.Name, alias)
			}
		}
		if !rc.Drop && rc.Template != "" && !renderer.Has(rc.Template) {
			return nil, fmt.Errorf("route %s: unknown template %q", rc.Name, rc.Template)
		}
		cm, err := CompileMatch(rc.Match)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		rt := route{cfg: rc, match: cm}
		if !ValidPriority(rc.Priority) {
			return nil, fmt.Errorf("route %s: priority %q must be one of %v", rc.Name, rc.Priority, Priorities)
		}
		if rc.Expr != "" {
			m, err := engines.Compile(rc.Engine, rc.Expr)
			if err != nil {
				return nil, fmt.Errorf("route %s: %w", rc.Name, err)
			}
			rt.expr = m
		}
		r.routes = append(r.routes, rt)
	}
	return r, nil
}

// explain reports whether the route applies and, if not, which condition failed.
func (r *Router) explain(rt *route, ev *Event) (bool, string) {
	if ok, why := rt.match.Explain(ev); !ok {
		return false, why
	}
	if rt.expr != nil {
		ok, err := rt.expr.Eval(ev)
		if err != nil { // typically a missing key, counts as no match
			return false, "expr error: " + err.Error()
		}
		if !ok {
			return false, "expr is false"
		}
	}
	return true, ""
}

func sortedKeys(m map[string]matcher) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TraceStep explains what one route did with an event.
type TraceStep struct {
	Route   string `json:"route"`
	Matched bool   `json:"matched"`
	Reason  string `json:"reason,omitempty"`
	Action  string `json:"action,omitempty"` // deliver, drop, skipped
}

type Result struct {
	Deliveries []Delivery
	DroppedBy  string
	Trace      []TraceStep
}

// Route walks the routes in order. The first match wins unless it sets
// continue: true. A matching drop route stops evaluation without output.
func (r *Router) Route(ev *Event) (Result, error) {
	var res Result
	stopped := false
	for i := range r.routes {
		rt := &r.routes[i]
		if stopped {
			res.Trace = append(res.Trace, TraceStep{Route: rt.cfg.Name, Action: "skipped"})
			continue
		}
		ok, reason := r.explain(rt, ev)
		if !ok {
			res.Trace = append(res.Trace, TraceStep{Route: rt.cfg.Name, Reason: reason})
			continue
		}
		if rt.cfg.Drop {
			res.DroppedBy = rt.cfg.Name
			res.Trace = append(res.Trace, TraceStep{Route: rt.cfg.Name, Matched: true, Action: "drop"})
			stopped = true
			continue
		}
		text, err := r.renderer.Render(rt.cfg.Template, ev)
		if err != nil {
			return res, fmt.Errorf("route %s: render: %w", rt.cfg.Name, err)
		}
		loud := rt.cfg.Loud || len(rt.cfg.Mention) > 0
		msg := Message{Markdown: text, Loud: loud, Priority: priority(rt.cfg.Priority, loud), Mentions: rt.cfg.Mention, Source: ev.Source, Route: rt.cfg.Name}
		for _, alias := range rt.cfg.To {
			d := r.Delivery(rt.cfg.Name, alias, msg)
			d.Digest = rt.cfg.Digest
			res.Deliveries = append(res.Deliveries, d)
		}
		res.Trace = append(res.Trace, TraceStep{Route: rt.cfg.Name, Matched: true, Action: "deliver"})
		if !rt.cfg.Continue {
			stopped = true
		}
	}
	return res, nil
}

// HasDestination reports whether alias is a configured destination.
func (r *Router) HasDestination(alias string) bool { _, ok := r.destinations[alias]; return ok }

// Delivery builds a delivery for a destination alias, with the
// destination's fallbacks attached.
func (r *Router) Delivery(routeName, alias string, msg Message) Delivery {
	if msg.Priority == "" {
		msg.Priority = priority("", msg.Loud)
	}
	d := r.destinations[alias]
	out := Delivery{Route: routeName, Destination: alias, Endpoint: d.Endpoint(), Address: d.Address, Message: msg}
	for _, fbAlias := range d.Fallback {
		fb := r.destinations[fbAlias]
		m := msg
		m.Markdown = msg.Markdown + FallbackNote(alias) // at the end: titles and subjects stay the alert's
		out.Fallback = append(out.Fallback, Delivery{Route: routeName, Destination: fbAlias, Endpoint: fb.Endpoint(),
			Address: fb.Address, Message: m, FallbackFor: alias})
	}
	return out
}

// FallbackNote is appended to a message that stands in for alias.
func FallbackNote(alias string) string {
	return fmt.Sprintf("\n\n↪ _Fallback: could not deliver to `%s`._", alias)
}

// PriorityRank orders priorities, unknown and empty count as normal.
func PriorityRank(p string) int {
	for i, x := range Priorities {
		if x == p {
			return i
		}
	}
	return 1
}

func priority(p string, loud bool) string {
	switch {
	case p != "":
		return p
	case loud:
		return PriorityHigh
	default:
		return PriorityNormal
	}
}
