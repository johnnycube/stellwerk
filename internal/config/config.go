// Package config is the configuration adapter: it reads YAML, validates it
// and maps it to the core's domain.Spec plus the settings adapters need.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen       string                 `yaml:"listen"`
	NodeID       string                 `yaml:"node_id"`
	Sources      map[string]Source      `yaml:"sources"`      // name -> source, "*" = fallback for unknown names
	Targets      map[string]Target      `yaml:"targets"`      // name -> target instance
	Destinations map[string]Destination `yaml:"destinations"` // alias -> target + address
	Templates    map[string]string      `yaml:"templates"`
	Engine       string                 `yaml:"engine"` // default expression engine for routes, "cel"
	Routes       []Route                `yaml:"routes"`
	Heartbeats   []Heartbeat            `yaml:"heartbeats"`
	Delivery     Delivery               `yaml:"delivery"`
	HA           HA                     `yaml:"ha"`
	UI           UI                     `yaml:"ui"`
	Log          Log                    `yaml:"log"`
	Metrics      Metrics                `yaml:"metrics"`
	Audit        Audit                  `yaml:"audit"`
	SelfPing     SelfPing               `yaml:"self_ping"`
	Canary       Canary                 `yaml:"canary"`
	Tests        []Test                 `yaml:"tests"`
}

// SelfPing reports to an outside service (healthchecks.io, Uptime Kuma
// push) that stellwerk is alive, so its own death does not go unnoticed.
type SelfPing struct {
	URL     string        `yaml:"url"`      // empty = off
	FailURL string        `yaml:"fail_url"` // default url + "/fail"
	Every   time.Duration `yaml:"every"`    // default 1m
}

// Canary sends a test message through the whole chain on a schedule. If it
// cannot be delivered, the destinations' fallbacks get it, which is the
// alarm. With verify_with, another Matrix account must also receive and
// decrypt it.
type Canary struct {
	To         StringList    `yaml:"to"`          // destinations to test, empty = off
	Every      time.Duration `yaml:"every"`       // default 24h
	At         string        `yaml:"at"`          // optional time of day "09:00" (local time zone), instead of every
	VerifyWith string        `yaml:"verify_with"` // "target/account" that must receive it, Matrix only
	Timeout    time.Duration `yaml:"timeout"`     // for verify_with, default 2m
	Alert      StringList    `yaml:"alert"`       // where a failed verification is reported, default: the fallbacks of to
}

// Test is a rule test run by "stellwerk -test": a payload and what the
// rules must do with it.
type Test struct {
	Name   string            `yaml:"name"`
	Source string            `yaml:"source"`
	Body   string            `yaml:"body"`
	Query  string            `yaml:"query"`
	Header map[string]string `yaml:"header"`
	Expect TestExpect        `yaml:"expect"`
}

// TestExpect: every field that is set must hold.
type TestExpect struct {
	Routes    []string `yaml:"routes"`     // routes that send, in order
	To        []string `yaml:"to"`         // destinations, in order
	DroppedBy string   `yaml:"dropped_by"` // the drop rule
	Unmatched bool     `yaml:"unmatched"`  // no rule matched
	Heartbeat string   `yaml:"heartbeat"`  // it is this heartbeat
	Contains  []string `yaml:"contains"`   // text in every rendered message
	Loud      *bool    `yaml:"loud"`
	Priority  string   `yaml:"priority"`
}

// Audit keeps the story of every received message: payload, the rules'
// verdict and every send attempt. Shown in the UI, optionally also written
// to a JSON lines file or the log.
type Audit struct {
	Enabled    *bool  `yaml:"enabled"`     // default true
	Size       int    `yaml:"size"`        // entries kept in memory, default 1000
	Payload    *bool  `yaml:"payload"`     // keep request bodies, default true
	PayloadMax int    `yaml:"payload_max"` // bytes per body, default 16384
	File       string `yaml:"file"`        // JSON lines of settled entries, e.g. /data/audit.jsonl
	FileMaxMB  int    `yaml:"file_max_mb"` // rotate to <file>.1 above this, default 50
	Log        bool   `yaml:"log"`         // also one log line per settled entry
}

func (a Audit) On() bool          { return a.Enabled == nil || *a.Enabled }
func (a Audit) KeepPayload() bool { return a.Payload == nil || *a.Payload }

// Metrics serves Prometheus metrics on their own listener. Empty = off.
type Metrics struct {
	Listen string `yaml:"listen"`
}

// Log configures logging. JSON is what most Kubernetes log stacks want.
type Log struct {
	Level  string `yaml:"level"`  // debug, info (default), warn, error
	Format string `yaml:"format"` // text (default) or json
}

// Source is one named sender. Kind selects the decoder (alertmanager, generic, ...).
type Source struct {
	Kind        string     `yaml:"kind"`
	Token       string     `yaml:"token"`
	TokenHeader string     `yaml:"token_header"` // e.g. X-Gitlab-Token, default: Authorization / ?token=
	Signature   *Signature `yaml:"signature"`    // HMAC-SHA256 of the body, instead of or in addition to token
	RateLimit   string     `yaml:"rate_limit"`   // e.g. "60/m", empty = unlimited
	Burst       int        `yaml:"burst"`        // default: the count of rate_limit
	Options     yaml.Node  `yaml:"options"`      // decoder-specific
}

// Signature checks an HMAC-SHA256 of the body. A preset fills header and
// prefix: github, gitea, forgejo. Without a preset, header is required.
type Signature struct {
	Preset string `yaml:"preset"`
	Header string `yaml:"header"`
	Prefix string `yaml:"prefix"`
	Secret string `yaml:"secret"`
}

// SignaturePresets are the header and prefix of well-known senders.
var SignaturePresets = map[string]Signature{
	"github":  {Header: "X-Hub-Signature-256", Prefix: "sha256="},
	"gitea":   {Header: "X-Gitea-Signature"},
	"forgejo": {Header: "X-Forgejo-Signature"},
}

// Resolved returns header and prefix with the preset applied.
func (s Signature) Resolved() (Signature, error) {
	if s.Preset != "" {
		p, ok := SignaturePresets[s.Preset]
		if !ok {
			return s, fmt.Errorf("unknown preset %q (known: github, gitea, forgejo)", s.Preset)
		}
		if s.Header == "" {
			s.Header = p.Header
		}
		if s.Prefix == "" {
			s.Prefix = p.Prefix
		}
	}
	if s.Header == "" {
		return s, fmt.Errorf("header or preset is required")
	}
	if s.Secret == "" {
		return s, fmt.Errorf("secret is required")
	}
	return s, nil
}

// Rate parses rate_limit into tokens per second and burst. ok is false when
// no limit is set.
func (s Source) Rate() (perSecond, burst float64, ok bool, err error) {
	spec := strings.TrimSpace(s.RateLimit)
	if spec == "" {
		return 0, 0, false, nil
	}
	n, unit, found := strings.Cut(spec, "/")
	count, perr := strconv.ParseFloat(strings.TrimSpace(n), 64)
	if !found || perr != nil || count <= 0 {
		return 0, 0, false, fmt.Errorf("rate_limit %q: want <count>/<s|m|h>, e.g. 60/m", spec)
	}
	per := map[string]float64{"s": 1, "m": 60, "h": 3600}[strings.TrimSpace(unit)]
	if per == 0 {
		return 0, 0, false, fmt.Errorf("rate_limit %q: unit must be s, m or h", spec)
	}
	burst = float64(s.Burst)
	if s.Burst <= 0 {
		burst = max(1, count)
	}
	return count / per, burst, true, nil
}

// Target is one configured output, e.g. a Matrix homeserver or a webhook
// receiver. Settings are shared by all accounts, each account's own settings
// are merged over them. Without accounts there is one account, "default".
type Target struct {
	Type     string               `yaml:"type"`
	Settings yaml.Node            `yaml:"settings"`
	Accounts map[string]yaml.Node `yaml:"accounts"`
}

// DefaultAccount is the implicit account of a target without accounts.
const DefaultAccount = "default"

// AccountNames returns the account names of a target, sorted.
func (t Target) AccountNames() []string {
	if len(t.Accounts) == 0 {
		return []string{DefaultAccount}
	}
	out := make([]string, 0, len(t.Accounts))
	for k := range t.Accounts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Destination is a named place messages go: an account of a target plus an
// address in it (Matrix room ID, URL path ...). Account may be omitted when
// the target has exactly one.
type Destination struct {
	Target  string `yaml:"target"`
	Account string `yaml:"account"`
	Address string `yaml:"address"`
	// Fallback destinations get the message when delivery here finally
	// fails, e.g. email when the Matrix homeserver is down.
	Fallback StringList `yaml:"fallback"`
}

type Route struct {
	Name     string     `yaml:"name"`
	Match    Match      `yaml:"match"`
	Expr     string     `yaml:"expr"`   // optional expression, ANDed with match
	Engine   string     `yaml:"engine"` // engine for expr, default: the top-level engine
	To       StringList `yaml:"to"`     // destination aliases
	Template string     `yaml:"template"`
	Loud     bool       `yaml:"loud"`     // target should notify (Matrix: m.text instead of m.notice)
	Priority string     `yaml:"priority"` // low, normal, high, urgent. Default: high if loud, else normal
	// Digest collects the route's messages per destination and sends one
	// summary per window, e.g. 1h. For noise, not for alerts.
	Digest   time.Duration `yaml:"digest"`
	Mention  []string      `yaml:"mention"` // target-specific identities to mention
	Drop     bool          `yaml:"drop"`
	Continue bool          `yaml:"continue"`
}

// Match: all given conditions must hold. Values are anchored regexes, a
// leading "!" negates. Source is a glob ("*", "sonarr", "*arr").
type Match struct {
	Source string            `yaml:"source"`
	Status string            `yaml:"status"`
	Labels map[string]string `yaml:"labels"`
	Fields map[string]string `yaml:"fields"`
}

// Heartbeat: an event that must keep arriving. It is consumed (not routed)
// and an alert goes out if it stops for longer than Timeout.
type Heartbeat struct {
	Name    string            `yaml:"name"`
	Source  string            `yaml:"source"`
	Labels  map[string]string `yaml:"labels"` // exact match, e.g. alertname: Watchdog
	Timeout time.Duration     `yaml:"timeout"`
	To      StringList        `yaml:"to"`
	Mention []string          `yaml:"mention"`
	// Token enables /ping/<name> and /ping/<name>/fail for cron jobs and
	// scripts, like healthchecks.io. A heartbeat needs a source, a token,
	// or both.
	Token string `yaml:"token"`
}

type Delivery struct {
	// sync: answer the sender only after delivery (non-2xx makes it retry,
	// possibly on another node). async: answer once queued.
	Mode       string        `yaml:"mode"`
	Timeout    time.Duration `yaml:"timeout"`
	QueueSize  int           `yaml:"queue_size"`
	MaxRetries int           `yaml:"max_retries"`
	MaxBackoff time.Duration `yaml:"max_backoff"`
	// Identical payloads from the same source within this window are delivered once.
	DedupeWindow time.Duration `yaml:"dedupe_window"`
	// On shutdown, how long to wait for running requests and queued
	// deliveries. Keep it below Kubernetes' terminationGracePeriodSeconds.
	DrainTimeout time.Duration `yaml:"drain_timeout"`
	// Spool is a SQLite file that keeps accepted deliveries until they are
	// finished, so they survive restarts and crashes. Empty = memory only.
	// {node} is replaced by node_id.
	Spool string `yaml:"spool"`
	// A message sent later than late_after after it came in gets a note
	// with its age (default 10m, "0s" = never). One older than max_age is
	// not sent at all (default: no limit).
	LateAfter *time.Duration `yaml:"late_after"`
	MaxAge    time.Duration  `yaml:"max_age"`
	// After breaker_failures deliveries in a row failed (default 3, -1 =
	// off), an account is skipped for breaker_cooldown (default 1m) and
	// fallbacks get messages at once.
	BreakerFailures int           `yaml:"breaker_failures"`
	BreakerCooldown time.Duration `yaml:"breaker_cooldown"`
}

// HA is optional. Disabled (default), the node coordinates with nobody.
type HA struct {
	Enabled  bool          `yaml:"enabled"`
	Peers    []Peer        `yaml:"peers"`  // all nodes incl. this one
	Secret   string        `yaml:"secret"` // shared bearer secret for /cluster/* calls
	Interval time.Duration `yaml:"interval"`
}

type Peer struct {
	ID  string `yaml:"id"`
	URL string `yaml:"url"` // base URL of the peer's main listener
}

type UI struct {
	Listen   *string `yaml:"listen"`
	Password string  `yaml:"password"`
}

// StringList accepts "a" or ["a", "b"].
type StringList []string

func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = StringList{n.Value}
		return nil
	}
	var l []string
	if err := n.Decode(&l); err != nil {
		return err
	}
	*s = l
	return nil
}

// Parse reads one YAML document, expands ${VAR} from the process
// environment, decodes strictly and validates. LoadFrom adds directories and
// STELLWERK_* variables on top.
func Parse(raw []byte) (*Config, error) {
	return decode([]byte(os.ExpandEnv(string(raw))))
}

func decode(b []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg.defaults()
	return &cfg, cfg.validate()
}

func (c *Config) defaults() {
	if c.Engine == "" {
		c.Engine = "cel"
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.NodeID == "" {
		c.NodeID, _ = os.Hostname()
	}
	if c.UI.Listen == nil {
		def := "127.0.0.1:8081"
		c.UI.Listen = &def
	}
	d := &c.Delivery
	if d.Mode == "" {
		d.Mode = "sync"
	}
	if d.Timeout == 0 {
		d.Timeout = 20 * time.Second
	}
	if d.QueueSize == 0 {
		d.QueueSize = 500
	}
	if d.MaxRetries == 0 {
		d.MaxRetries = 5
	}
	if d.MaxBackoff == 0 {
		d.MaxBackoff = time.Minute
	}
	if d.DrainTimeout == 0 {
		d.DrainTimeout = 25 * time.Second
	}
	d.Spool = strings.ReplaceAll(d.Spool, "{node}", c.NodeID)
	if d.LateAfter == nil {
		late := 10 * time.Minute
		d.LateAfter = &late
	}
	if d.BreakerFailures == 0 {
		d.BreakerFailures = 3
	}
	if d.BreakerCooldown == 0 {
		d.BreakerCooldown = time.Minute
	}
	if c.Audit.Size == 0 {
		c.Audit.Size = 1000
	}
	if c.Audit.PayloadMax == 0 {
		c.Audit.PayloadMax = 16 << 10
	}
	if c.Audit.FileMaxMB == 0 {
		c.Audit.FileMaxMB = 50
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	if c.SelfPing.Every == 0 {
		c.SelfPing.Every = time.Minute
	}
	if c.Canary.Every == 0 {
		c.Canary.Every = 24 * time.Hour
	}
	if c.Canary.Timeout == 0 {
		c.Canary.Timeout = 2 * time.Minute
	}
	for i := range c.Tests {
		if c.Tests[i].Name == "" {
			c.Tests[i].Name = fmt.Sprintf("test-%d", i+1)
		}
	}
	if c.HA.Interval == 0 {
		c.HA.Interval = 3 * time.Second
	}
	for alias, d := range c.Destinations {
		if d.Account == "" {
			if t, ok := c.Targets[d.Target]; ok && len(t.AccountNames()) == 1 {
				d.Account = t.AccountNames()[0]
				c.Destinations[alias] = d
			}
		}
	}
	for i := range c.Routes {
		if c.Routes[i].Name == "" {
			c.Routes[i].Name = fmt.Sprintf("route-%d", i+1)
		}
	}
	for i := range c.Heartbeats {
		h := &c.Heartbeats[i]
		if h.Name == "" {
			h.Name = fmt.Sprintf("heartbeat-%d", i+1)
		}
		if h.Timeout == 0 {
			h.Timeout = 10 * time.Minute
		}
	}
}

func (c *Config) validate() error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if len(c.Sources) == 0 {
		add("sources: at least one source is required")
	}
	for name, s := range c.Sources {
		if s.Kind == "" {
			add("sources.%s: kind is required", name)
		}
		if s.Token == "" && s.Signature == nil {
			add("sources.%s: token or signature is required", name)
		}
		if s.Signature != nil {
			if _, err := s.Signature.Resolved(); err != nil {
				add("sources.%s.signature: %v", name, err)
			}
		}
		if _, _, _, err := s.Rate(); err != nil {
			add("sources.%s: %v", name, err)
		}
	}
	if len(c.Targets) == 0 {
		add("targets: at least one target is required")
	}
	for name, t := range c.Targets {
		if t.Type == "" {
			add("targets.%s: type is required", name)
		}
	}
	for alias, d := range c.Destinations {
		t, ok := c.Targets[d.Target]
		if !ok {
			add("destinations.%s: unknown target %q", alias, d.Target)
			continue
		}
		switch {
		case d.Account == "":
			add("destinations.%s: target %s has several accounts, set account (one of %v)", alias, d.Target, t.AccountNames())
		case !slices.Contains(t.AccountNames(), d.Account):
			add("destinations.%s: target %s has no account %q (has %v)", alias, d.Target, d.Account, t.AccountNames())
		}
		for _, fb := range d.Fallback {
			if fb == alias {
				add("destinations.%s: cannot be its own fallback", alias)
			} else if _, ok := c.Destinations[fb]; !ok {
				add("destinations.%s: unknown fallback %q", alias, fb)
			}
		}
	}
	checkTo := func(where string, to []string) {
		for _, alias := range to {
			if _, ok := c.Destinations[alias]; !ok {
				add("%s: unknown destination %q", where, alias)
			}
		}
	}
	if len(c.Routes) == 0 {
		add("routes: at least one route is required")
	}
	seen := map[string]bool{}
	for _, r := range c.Routes {
		if seen[r.Name] {
			add("routes: duplicate name %q", r.Name)
		}
		seen[r.Name] = true
		if !r.Drop && len(r.To) == 0 {
			add("routes.%s: to is required unless drop: true", r.Name)
		}
		checkTo("routes."+r.Name, r.To)
		if r.Digest < 0 || (r.Digest > 0 && r.Drop) {
			add("routes.%s: digest must be positive and needs a route that sends", r.Name)
		}
		if !domain.ValidPriority(r.Priority) {
			add("routes.%s: priority must be one of %v", r.Name, domain.Priorities)
		}
	}
	for _, h := range c.Heartbeats {
		if (h.Source == "" && h.Token == "") || len(h.To) == 0 {
			add("heartbeats.%s: to and a source or a token are required", h.Name)
		}
		checkTo("heartbeats."+h.Name, h.To)
	}
	checkTo("canary.to", c.Canary.To)
	checkTo("canary.alert", c.Canary.Alert)
	if c.Canary.At != "" {
		if _, err := time.Parse("15:04", c.Canary.At); err != nil {
			add("canary.at must be a time of day like 09:00")
		}
	}
	if v := c.Canary.VerifyWith; v != "" {
		t, acc, _ := strings.Cut(v, "/")
		if tc, ok := c.Targets[t]; !ok || tc.Type != "matrix" || (acc != "" && !slices.Contains(tc.AccountNames(), acc)) {
			add("canary.verify_with: %q must be a Matrix target/account", v)
		}
	}
	if u := c.SelfPing.URL; u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		add("self_ping.url must start with http:// or https://")
	}
	tests := map[string]bool{}
	for _, tc := range c.Tests {
		if tests[tc.Name] {
			add("tests: duplicate name %q", tc.Name)
		}
		tests[tc.Name] = true
		if tc.Source == "" {
			add("tests.%s: source is required", tc.Name)
		}
		checkTo("tests."+tc.Name+".expect.to", tc.Expect.To)
	}
	if !slices.Contains([]string{"debug", "info", "warn", "error"}, c.Log.Level) {
		add("log.level must be debug, info, warn or error")
	}
	if c.Log.Format != "text" && c.Log.Format != "json" {
		add("log.format must be text or json")
	}
	if c.Delivery.Mode != "sync" && c.Delivery.Mode != "async" {
		add("delivery.mode must be sync or async")
	}
	if c.HA.Enabled {
		if c.HA.Secret == "" {
			add("ha.secret is required when ha is enabled")
		}
		if len(c.HA.Peers) < 2 {
			add("ha.peers needs at least two nodes")
		}
		self := false
		for _, p := range c.HA.Peers {
			if p.ID == "" || p.URL == "" {
				add("ha.peers: id and url are required")
			}
			self = self || p.ID == c.NodeID
		}
		if !self {
			add("ha.peers must contain this node (node_id %q)", c.NodeID)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("config:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// SourceFor returns the config for a source name, falling back to "*".
func (c *Config) SourceFor(name string) (Source, bool) {
	if s, ok := c.Sources[name]; ok {
		return s, true
	}
	s, ok := c.Sources["*"]
	return s, ok
}

// Spec maps the config to what the core needs.
func (c *Config) Spec() domain.Spec {
	spec := domain.Spec{
		Engine: c.Engine, Destinations: map[string]domain.Destination{}, Templates: c.Templates,
		Delivery: domain.DeliveryPolicy{
			Sync: c.Delivery.Mode == "sync", Timeout: c.Delivery.Timeout, QueueSize: c.Delivery.QueueSize,
			MaxRetries: c.Delivery.MaxRetries, MaxBackoff: c.Delivery.MaxBackoff, DedupeWindow: c.Delivery.DedupeWindow,
			LateAfter: *c.Delivery.LateAfter, MaxAge: c.Delivery.MaxAge,
			BreakerFailures: max(c.Delivery.BreakerFailures, 0), BreakerCooldown: c.Delivery.BreakerCooldown,
		},
	}
	for _, r := range c.Routes {
		spec.Rules = append(spec.Rules, domain.Rule{
			Name: r.Name, Expr: r.Expr, Engine: r.Engine, To: r.To, Template: r.Template, Loud: r.Loud, Priority: r.Priority, Digest: r.Digest,
			Mention: r.Mention, Drop: r.Drop, Continue: r.Continue,
			Match: domain.Match{Source: r.Match.Source, Status: r.Match.Status, Labels: r.Match.Labels, Fields: r.Match.Fields},
		})
	}
	for alias, d := range c.Destinations {
		spec.Destinations[alias] = domain.Destination{Alias: alias, Target: d.Target, Account: d.Account, Address: d.Address, Fallback: d.Fallback}
	}
	for _, h := range c.Heartbeats {
		spec.Heartbeats = append(spec.Heartbeats, domain.Heartbeat{
			Name: h.Name, Source: h.Source, Labels: h.Labels,
			Timeout: h.Timeout, To: h.To, Mention: h.Mention,
		})
	}
	return spec
}
