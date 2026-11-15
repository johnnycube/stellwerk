package http

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	htmlpkg "html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"maunium.net/go/mautrix/format"
)

//go:embed ui/index.html
var uiHTML []byte

// UIHandler serves the dataflow UI and its API on their own listener.
func (s *Server) UIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		w.Write(uiHTML)
	})
	mux.HandleFunc("GET /api/flows", s.handleFlows)
	mux.HandleFunc("POST /api/simulate", s.handleSimulate)
	mux.HandleFunc("GET /api/audit", s.handleAuditList)
	mux.HandleFunc("GET /api/audit/{id}", s.handleAuditGet)
	mux.HandleFunc("POST /api/audit/{id}/resend", s.action(s.handleResend))
	mux.HandleFunc("GET /api/silences", s.handleSilenceList)
	mux.HandleFunc("POST /api/silences", s.action(s.handleSilenceAdd))
	mux.HandleFunc("DELETE /api/silences/{id}", s.action(s.handleSilenceDelete))
	mux.HandleFunc("POST /api/reload", s.action(s.handleReload))
	mux.HandleFunc("POST /api/canary", s.action(s.handleCanary))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if pw := s.UIPassword; pw != "" {
			_, got, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(pw)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="stellwerk"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

type flowSource struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	Auth     string `json:"auth"` // own, fallback
}

type flowDestination struct {
	Alias    string   `json:"alias"`
	Target   string   `json:"target"`
	Account  string   `json:"account"`
	Type     string   `json:"type"`
	Address  string   `json:"address"`
	Fallback []string `json:"fallback,omitempty"`
}

type flowRoute struct {
	Index    int               `json:"index"`
	Name     string            `json:"name"`
	Source   string            `json:"source,omitempty"`
	Status   string            `json:"status,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Fields   map[string]string `json:"fields,omitempty"`
	Expr     string            `json:"expr,omitempty"`
	Engine   string            `json:"engine,omitempty"` // set when expr is
	Priority string            `json:"priority,omitempty"`
	Digest   string            `json:"digest,omitempty"`
	To       []string          `json:"to,omitempty"`
	Template string            `json:"template,omitempty"`
	Loud     bool              `json:"loud"`
	Mention  []string          `json:"mention,omitempty"`
	Drop     bool              `json:"drop"`
	Continue bool              `json:"continue"`
}

type flowHeartbeat struct {
	Name     string            `json:"name"`
	Source   string            `json:"source"`
	Labels   map[string]string `json:"labels,omitempty"`
	Timeout  string            `json:"timeout"`
	To       []string          `json:"to"`
	LastSeen string            `json:"lastSeen"`
	Missing  bool              `json:"missing"`
	Failed   bool              `json:"failed"`
	Ping     bool              `json:"ping"` // has /ping/<name>
}

type flowTemplate struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Builtin bool   `json:"builtin"`
}

type flowsResponse struct {
	Sources      []flowSource        `json:"sources"`
	Kinds        []source.Info       `json:"kinds"`
	Targets      []TargetView        `json:"targets"`
	Destinations []flowDestination   `json:"destinations"`
	Routes       []flowRoute         `json:"routes"`
	Heartbeats   []flowHeartbeat     `json:"heartbeats"`
	Templates    []flowTemplate      `json:"templates"`
	Cluster      ports.ClusterStatus `json:"cluster"`
	Stats        ports.StatsSnapshot `json:"stats"`
	Silences     []domain.Silence    `json:"silences"`
	Actions      bool                `json:"actions"` // silences, resend and reload are available
	Canary       *ports.CanaryResult `json:"canary,omitempty"`
	CanaryOn     bool                `json:"canaryOn"`
}

func (s *Server) handleFlows(w http.ResponseWriter, _ *http.Request) {
	topo := s.Describe.Describe()
	resp := flowsResponse{
		Kinds: source.Infos(), Cluster: topo.Cluster, Stats: topo.Stats,
		Routes: []flowRoute{}, Heartbeats: []flowHeartbeat{}, Destinations: []flowDestination{}, Targets: []TargetView{},
		Silences: topo.Silences, Actions: s.Silences != nil,
	}
	if resp.Silences == nil {
		resp.Silences = []domain.Silence{}
	}
	if s.Canary != nil {
		resp.CanaryOn = true
		if last, ok := s.Canary.Last(); ok {
			resp.Canary = &last
		}
	}

	types := map[string]string{}
	if s.Targets != nil {
		resp.Targets = s.Targets()
		for _, t := range resp.Targets {
			types[t.Target] = t.Type
		}
	}

	names := map[string]bool{}
	for _, n := range s.sources().Names() {
		names[n] = true
	}
	for _, r := range topo.Rules {
		if r.Match.Source != "" && !strings.ContainsAny(r.Match.Source, "*?[") {
			names[r.Match.Source] = true
		}
	}
	for _, h := range topo.Heartbeats {
		if h.Source != "" { // ping-only heartbeats have none
			names[h.Source] = true
		}
	}
	for n := range topo.Stats.Sources {
		names[n] = true
	}
	for name := range names {
		src, own, ok := s.sources().Resolve(name)
		if !ok {
			continue
		}
		auth := "own"
		if !own {
			auth = "fallback"
		}
		resp.Sources = append(resp.Sources, flowSource{Name: name, Kind: src.Kind, Endpoint: "/in/" + name, Auth: auth})
	}
	sort.Slice(resp.Sources, func(i, j int) bool { return resp.Sources[i].Name < resp.Sources[j].Name })

	for _, d := range topo.Destinations {
		resp.Destinations = append(resp.Destinations, flowDestination{Alias: d.Alias, Target: d.Target, Account: d.Account, Type: types[d.Target], Address: d.Address, Fallback: d.Fallback})
	}
	for i, r := range topo.Rules {
		fr := flowRoute{
			Index: i, Name: r.Name, Source: r.Match.Source, Status: r.Match.Status, Labels: r.Match.Labels,
			Fields: r.Match.Fields, Expr: r.Expr, To: r.To, Template: r.Template, Loud: r.Loud || len(r.Mention) > 0,
			Mention: r.Mention, Drop: r.Drop, Continue: r.Continue,
		}
		if r.Expr != "" {
			fr.Engine = r.Engine
			if fr.Engine == "" {
				fr.Engine = topo.Engine
			}
		}
		if !r.Drop {
			fr.Priority = r.Priority
			if fr.Priority == "" {
				fr.Priority = map[bool]string{true: domain.PriorityHigh, false: domain.PriorityNormal}[fr.Loud]
			}
		}
		if r.Digest > 0 {
			fr.Digest = r.Digest.String()
		}
		if !r.Drop && fr.Template == "" {
			fr.Template = "auto"
		}
		resp.Routes = append(resp.Routes, fr)
	}
	for _, h := range topo.Heartbeats {
		resp.Heartbeats = append(resp.Heartbeats, flowHeartbeat{
			Name: h.Name, Source: h.Source, Labels: h.Labels,
			Timeout: h.Timeout.String(), To: h.To, LastSeen: h.LastSeen.Format("2006-01-02T15:04:05.000Z07:00"), Missing: h.Missing,
			Failed: h.Failed, Ping: s.pingTokens()[h.Name] != "",
		})
	}
	for _, t := range topo.Templates {
		resp.Templates = append(resp.Templates, flowTemplate(t))
	}
	writeJSON(w, resp)
}

type simulateRequest struct {
	Source string `json:"source"`
	Query  string `json:"query"`
	Body   string `json:"body"`
}

type simulatedDelivery struct {
	Route       string   `json:"route"`
	Destination string   `json:"destination"`
	Endpoint    string   `json:"endpoint"`
	Markdown    string   `json:"markdown"`
	HTML        string   `json:"html"`
	Loud        bool     `json:"loud"`
	Mentions    []string `json:"mentions,omitempty"`
}

type simulatedEvent struct {
	Status     string              `json:"status,omitempty"`
	Labels     map[string]string   `json:"labels,omitempty"`
	Heartbeat  string              `json:"heartbeat,omitempty"`
	Trace      []domain.TraceStep  `json:"trace"`
	Deliveries []simulatedDelivery `json:"deliveries"`
	DroppedBy  string              `json:"droppedBy,omitempty"`
	Silenced   string              `json:"silenced,omitempty"`
}

type simulateResponse struct {
	Kind   string           `json:"kind,omitempty"`
	Events []simulatedEvent `json:"events"`
	Error  string           `json:"error,omitempty"`
}

func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	var req simulateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeJSON(w, simulateResponse{Error: "invalid request: " + err.Error()})
		return
	}
	if !validSource.MatchString(req.Source) {
		writeJSON(w, simulateResponse{Error: "Source must be 1 to 64 characters: letters, digits, dot, dash or underscore."})
		return
	}
	src, _, ok := s.sources().Resolve(req.Source)
	if !ok {
		writeJSON(w, simulateResponse{Error: "No source with this name and no \"*\" fallback, the request would get 404."})
		return
	}
	q, _ := url.ParseQuery(strings.TrimPrefix(req.Query, "?"))
	events, err := src.Decoder.Decode(source.Input{Source: req.Source, Body: []byte(req.Body), Query: q})
	out := simulateResponse{Kind: src.Kind, Events: []simulatedEvent{}}
	if err != nil {
		out.Error = err.Error()
		writeJSON(w, out)
		return
	}
	for _, sim := range s.Simulate.Simulate(events) {
		se := simulatedEvent{
			Status: sim.Event.Status, Labels: sim.Event.Labels, Heartbeat: sim.Heartbeat,
			Trace: sim.Trace, DroppedBy: sim.DroppedBy, Silenced: sim.Silenced, Deliveries: []simulatedDelivery{},
		}
		if se.Trace == nil {
			se.Trace = []domain.TraceStep{}
		}
		if sim.Err != "" {
			out.Error = sim.Err
		}
		for _, d := range sim.Deliveries {
			se.Deliveries = append(se.Deliveries, simulatedDelivery{
				Route: d.Route, Destination: d.Destination, Endpoint: d.Endpoint,
				Markdown: d.Message.Markdown, HTML: previewHTML(d.Message), Loud: d.Message.Loud, Mentions: d.Message.Mentions,
			})
		}
		out.Events = append(out.Events, se)
	}
	writeJSON(w, out)
}

// previewHTML renders Markdown like Matrix would, raw HTML stays escaped.
func previewHTML(m domain.Message) string {
	md := m.Markdown
	if len(m.Mentions) > 0 {
		md = strings.Join(m.Mentions, " ") + " " + md
	}
	rendered := format.RenderMarkdown(md, true, false)
	if rendered.FormattedBody != "" {
		return rendered.FormattedBody
	}
	return strings.ReplaceAll(htmlpkg.EscapeString(rendered.Body), "\n", "<br>")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// auditSummary is a list row: everything but payload and message bodies.
type auditSummary struct {
	ID           string            `json:"id"`
	Time         string            `json:"time"`
	Node         string            `json:"node,omitempty"`
	Source       string            `json:"source"`
	Kind         string            `json:"kind,omitempty"`
	Status       string            `json:"status,omitempty"`
	Verdict      string            `json:"verdict"`
	Reason       string            `json:"reason,omitempty"`
	Rules        []string          `json:"rules,omitempty"`
	Destinations []destinationMark `json:"destinations,omitempty"`
}

type destinationMark struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Digest   bool   `json:"digest,omitempty"`
}

func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	if s.Audit == nil {
		writeJSON(w, map[string]any{"enabled": false, "entries": []auditSummary{}})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	entries := s.Audit.List(ports.AuditQuery{Source: q.Get("source"), Verdict: q.Get("verdict"), Text: q.Get("q"), Limit: limit})
	out := make([]auditSummary, 0, len(entries))
	for _, e := range entries {
		sum := auditSummary{
			ID: e.ID, Time: e.Time.Format("2006-01-02T15:04:05.000Z07:00"), Node: e.Node, Source: e.Source, Kind: e.Kind,
			Status: e.Status, Verdict: e.Verdict, Reason: e.Reason,
		}
		for _, t := range e.Trace {
			if t.Matched {
				sum.Rules = append(sum.Rules, t.Route)
			}
		}
		for _, d := range e.Deliveries {
			sum.Destinations = append(sum.Destinations, destinationMark{Name: d.Destination, Status: d.Status, Attempts: len(d.Attempts), Digest: d.Digest != ""})
		}
		out = append(out, sum)
	}
	writeJSON(w, map[string]any{"enabled": true, "entries": out})
}

func (s *Server) handleAuditGet(w http.ResponseWriter, r *http.Request) {
	if s.Audit == nil {
		http.Error(w, "audit log is off", http.StatusNotFound)
		return
	}
	e, ok := s.Audit.Get(r.PathValue("id"))
	if !ok {
		http.Error(w, "no such entry, it may have been rotated out", http.StatusNotFound)
		return
	}
	type delivery struct {
		ports.AuditDelivery
		HTML string `json:"html"`
	}
	resp := struct {
		ports.AuditEntry
		Deliveries []delivery `json:"deliveries"`
	}{AuditEntry: e}
	for _, d := range e.Deliveries {
		resp.Deliveries = append(resp.Deliveries, delivery{AuditDelivery: d, HTML: previewHTML(domain.Message{Markdown: d.Message})})
	}
	writeJSON(w, resp)
}
