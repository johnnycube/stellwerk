package config

import (
	"strings"
	"testing"
	"time"
)

const base = `
node_id: a
sources: { am: { kind: alertmanager, token: t } }
targets: { m: { type: matrix, settings: { homeserver: x } } }
destinations: { crit: { target: m, address: "!r:x" } }
`

func TestParseValid(t *testing.T) {
	cfg, err := Parse([]byte(base + `
routes:
  - { match: { source: am }, to: crit }
  - { name: multi, to: [crit, crit] }
heartbeats: [ { source: am, labels: { alertname: Watchdog }, to: crit } ]
ha: { enabled: true, secret: s, peers: [ { id: a, url: "http://a" }, { id: b, url: "http://b" } ] }
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Name != "route-1" || len(cfg.Routes[1].To) != 2 || cfg.Routes[0].To[0] != "crit" {
		t.Errorf("routes: %+v", cfg.Routes)
	}
	if cfg.Delivery.Mode != "sync" || cfg.Heartbeats[0].Timeout.Minutes() != 10 || *cfg.UI.Listen != "127.0.0.1:8081" {
		t.Errorf("defaults: %+v %+v", cfg.Delivery, cfg.Heartbeats)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"unknown destination":    `routes: [ { to: nope } ]`,
		"to is required":         `routes: [ { name: x } ]`,
		"must contain this node": "routes: [ { to: crit } ]\nha: { enabled: true, secret: s, peers: [ { id: b, url: http://b }, { id: c, url: http://c } ] }",
		"ha.secret":              "routes: [ { to: crit } ]\nha: { enabled: true, peers: [ { id: a, url: http://a }, { id: b, url: http://b } ] }",
		"at least two nodes":     "routes: [ { to: crit } ]\nha: { enabled: true, secret: s, peers: [ { id: a, url: http://a } ] }",
		"delivery.mode":          "routes: [ { to: crit } ]\ndelivery: { mode: maybe }",
		"field nope not found":   "routes: [ { to: crit } ]\nnope: 1",
		"duplicate name":         "routes: [ { name: x, to: crit }, { name: x, to: crit } ]",
	}
	for want, extra := range cases {
		_, err := Parse([]byte(base + extra))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestEnvExpansionAndFallbackSource(t *testing.T) {
	t.Setenv("TOK", "secret")
	cfg, err := Parse([]byte(`
sources: { "*": { kind: generic, token: ${TOK} } }
targets: { m: { type: matrix } }
destinations: { d: { target: m } }
routes: [ { to: d } ]`))
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := cfg.SourceFor("anything"); !ok || s.Token != "secret" {
		t.Errorf("fallback: %+v", s)
	}
}

func TestAccounts(t *testing.T) {
	multi := `
sources: { "*": { kind: generic, token: x } }
targets:
  m:
    type: matrix
    settings: { homeserver: hs }
    accounts:
      alerts: { user_id: "@a:x" }
      media: { user_id: "@m:x" }
  hook: { type: webhook, settings: { url: http://x } }
`
	cfg, err := Parse([]byte(multi + `
destinations:
  crit: { target: m, account: alerts, address: "!r:x" }
  tv: { target: m, account: media, address: "!t:x" }
  pager: { target: hook }
routes: [ { to: [crit, tv, pager] } ]`))
	if err != nil {
		t.Fatal(err)
	}
	spec := cfg.Spec()
	if spec.Destinations["pager"].Account != DefaultAccount || spec.Destinations["tv"].Endpoint() != "m/media" {
		t.Errorf("destinations: %+v", spec.Destinations)
	}
	if got := cfg.Targets["m"].AccountNames(); len(got) != 2 || got[0] != "alerts" {
		t.Errorf("accounts: %v", got)
	}
	for want, extra := range map[string]string{
		"set account": "destinations: { crit: { target: m, address: x } }\nroutes: [ { to: crit } ]",
		"no account":  "destinations: { crit: { target: m, account: nope } }\nroutes: [ { to: crit } ]",
	} {
		if _, err := Parse([]byte(multi + extra)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestHADisabledByDefault(t *testing.T) {
	cfg, err := Parse([]byte(base + "routes: [ { to: crit } ]\nha: { peers: [ { id: x, url: http://x } ] }"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HA.Enabled || !cfg.Spec().Delivery.Sync {
		t.Errorf("ha should be off, delivery sync: %+v", cfg.HA)
	}
}

func TestSourceRate(t *testing.T) {
	cases := map[string][2]float64{"60/m": {1, 60}, "5/s": {5, 5}, "3600/h": {1, 3600}, " 2 / s ": {2, 2}}
	for spec, want := range cases {
		r, b, ok, err := Source{RateLimit: spec}.Rate()
		if err != nil || !ok || r != want[0] || b != want[1] {
			t.Errorf("%s: %v %v %v %v", spec, r, b, ok, err)
		}
	}
	if _, b, _, _ := (Source{RateLimit: "60/m", Burst: 5}).Rate(); b != 5 {
		t.Errorf("explicit burst %v", b)
	}
	if _, _, ok, err := (Source{}).Rate(); ok || err != nil {
		t.Error("empty means unlimited")
	}
	for _, bad := range []string{"60", "x/m", "0/s", "-1/s", "5/d"} {
		if _, _, _, err := (Source{RateLimit: bad}).Rate(); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	bad := strings.Replace(base, "token: t }", "token: t, rate_limit: 5/d }", 1) + "routes: [ { to: crit } ]"
	if _, err := Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "unit must be") {
		t.Errorf("validation: %v", err)
	}
}

func TestFallbackPriorityEngineSpool(t *testing.T) {
	head := "node_id: n1\nsources: { am: { kind: alertmanager, token: t } }\ntargets: { m: { type: matrix }, mail: { type: email } }\n"
	cfg, err := Parse([]byte(head + `
engine: cel
destinations:
  crit: { target: m, address: "!r:x", fallback: mail }
  mail: { target: mail, address: me@x }
routes: [ { to: crit, priority: urgent, engine: cel, expr: "true" } ]
delivery: { spool: "/data/spool-{node}.db" }
`))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Spec()
	if d := s.Destinations["crit"]; len(d.Fallback) != 1 || d.Fallback[0] != "mail" {
		t.Errorf("%+v", d)
	}
	if r := s.Rules[0]; r.Priority != "urgent" || r.Engine != "cel" {
		t.Errorf("%+v", r)
	}
	if cfg.Delivery.Spool != "/data/spool-n1.db" || cfg.Engine != "cel" {
		t.Errorf("%q %q", cfg.Delivery.Spool, cfg.Engine)
	}
	if def, _ := Parse([]byte(head + "destinations: { crit: { target: m } }\nroutes: [ { to: crit } ]\n")); def.Engine != "cel" || def.Delivery.Spool != "" {
		t.Error("defaults")
	}
	for want, extra := range map[string]string{
		"unknown fallback":        "destinations: { crit: { target: m, fallback: nope } }\nroutes: [ { to: crit } ]",
		"cannot be its own":       "destinations: { crit: { target: m, fallback: crit } }\nroutes: [ { to: crit } ]",
		"priority must be one of": "destinations: { crit: { target: m } }\nroutes: [ { to: crit, priority: asap } ]",
	} {
		if _, err := Parse([]byte(head + extra)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestSignatures(t *testing.T) {
	rest := "targets: { m: { type: matrix } }\ndestinations: { crit: { target: m } }\nroutes: [ { to: crit } ]\n"
	cfg, err := Parse([]byte(rest + `sources:
  gh: { kind: generic, signature: { preset: github, secret: s } }
  fj: { kind: generic, signature: { preset: forgejo, secret: s } }
  own: { kind: generic, token: t, signature: { header: X-Sig, prefix: "v1=", secret: s } }
  gl: { kind: generic, token: t, token_header: X-Gitlab-Token }
`))
	if err != nil {
		t.Fatal(err)
	}
	gh, _ := cfg.Sources["gh"].Signature.Resolved()
	fj, _ := cfg.Sources["fj"].Signature.Resolved()
	own, _ := cfg.Sources["own"].Signature.Resolved()
	if gh.Header != "X-Hub-Signature-256" || gh.Prefix != "sha256=" || fj.Header != "X-Forgejo-Signature" || own.Prefix != "v1=" ||
		cfg.Sources["gl"].TokenHeader != "X-Gitlab-Token" {
		t.Errorf("%+v %+v %+v", gh, fj, own)
	}
	if !sensitive("secret") || sensitive("token_header") {
		t.Error("secrets are redacted, header names are not")
	}
	for want, src := range map[string]string{
		"token or signature is required": "{ kind: generic }",
		"unknown preset":                 "{ kind: generic, signature: { preset: gitlab, secret: s } }",
		"header or preset":               "{ kind: generic, signature: { secret: s } }",
		"secret is required":             "{ kind: generic, signature: { preset: github } }",
	} {
		if _, err := Parse([]byte(rest + "sources: { x: " + src + " }\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
	}
}

func TestDeliveryHardeningDefaults(t *testing.T) {
	head := "sources: { am: { kind: alertmanager, token: t } }\ntargets: { m: { type: matrix } }\ndestinations: { crit: { target: m } }\nroutes: [ { to: crit } ]\n"
	cfg, err := Parse([]byte(head))
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Spec().Delivery
	if p.LateAfter != 10*time.Minute || p.MaxAge != 0 || p.BreakerFailures != 3 || p.BreakerCooldown != time.Minute {
		t.Errorf("%+v", p)
	}
	cfg, _ = Parse([]byte(head + "delivery: { late_after: 0s, max_age: 6h, breaker_failures: -1 }"))
	if p := cfg.Spec().Delivery; p.LateAfter != 0 || p.MaxAge != 6*time.Hour || p.BreakerFailures != 0 {
		t.Errorf("%+v", p)
	}
}

func TestSchema(t *testing.T) {
	b, err := Schema([]string{"alertmanager", "generic"}, []string{"matrix"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"$schema"`, `"#/$defs/Source"`, `"enum": [`, `"alertmanager"`, `"pattern"`, `"additionalProperties": false`,
		`Identical payloads from the same source within this window are delivered once.`, `"self_ping"`, `"tests"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s", want)
		}
	}
}
