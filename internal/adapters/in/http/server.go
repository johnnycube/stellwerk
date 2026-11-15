// Package http is the driving HTTP adapter: webhook ingest, health, the
// dataflow UI and its API. It translates between HTTP and the core's ports
// and owns everything protocol-specific: tokens, decoders, status codes.
package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

const maxBody = 1 << 20

var validSource = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

// Source is one configured sender: its decoder, how it proves who it is,
// and an optional limit. Token and Signature may both be set, then both
// must hold. At least one is required (config checks that).
type Source struct {
	Kind        string
	Token       string
	TokenHeader string // read the token from this header instead (X-Gitlab-Token ...)
	Signature   *Signature
	Decoder     source.Decoder
	RateLimit   *RateLimit // nil = unlimited
}

// Signature is an HMAC-SHA256 of the body, hex encoded, in a header. This
// is how GitHub, Gitea, Forgejo and many others sign webhooks: the secret
// never travels with the request.
type Signature struct {
	Header string // e.g. X-Hub-Signature-256
	Prefix string // e.g. "sha256="
	Secret string
}

// Verify reports whether the request's signature matches body.
func (s Signature) Verify(h http.Header, body []byte) bool {
	got := h.Get(s.Header)
	if s.Secret == "" || !strings.HasPrefix(got, s.Prefix) {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(got, s.Prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.Secret))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

// Sources by name, "*" is the fallback for names not listed.
type Sources map[string]Source

// Resolve returns the source config for a name and whether it is its own entry.
func (s Sources) Resolve(name string) (Source, bool, bool) {
	if src, ok := s[name]; ok {
		return src, true, true
	}
	src, ok := s["*"]
	return src, false, ok
}

func (s Sources) Names() []string {
	out := make([]string, 0, len(s))
	for k := range s {
		if k != "*" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// TargetView describes an endpoint for the UI.
type TargetView struct {
	Target   string   `json:"target"`
	Account  string   `json:"account"`
	Type     string   `json:"type"`
	Ready    bool     `json:"ready"`
	Problems []string `json:"problems,omitempty"`
}

// Server only talks to the core through use case ports.
type Server struct {
	Ingest     ports.IngestEvents
	Simulate   ports.SimulateEvents
	Describe   ports.DescribeTopology
	Audit      ports.ReadAudit
	Silences   ports.ManageSilences // nil: silences are not offered
	Resend     ports.ResendAudit    // nil: no "send again"
	Reload     func() error         // nil: no reload from the UI
	Pings      ports.ReceivePings   // nil: no /ping
	Canary     ports.RunCanary      // nil: no canary
	PingTokens map[string]string    // heartbeat name -> token
	Sources    Sources
	Ready      func() bool
	Targets    func() []TargetView
	UIPassword string
	Mounts     map[string]http.Handler // extra handlers on the main listener, e.g. /cluster/

	// OnRateLimited is called when a request is refused, e.g. to count it.
	OnRateLimited func(source string)

	limitOnce sync.Once
	limits    *limiter
	swapped   atomic.Pointer[Sources]
	pingSwap  atomic.Pointer[map[string]string]
}

// SetPingTokens switches to reloaded heartbeat tokens.
func (s *Server) SetPingTokens(t map[string]string) { s.pingSwap.Store(&t) }

func (s *Server) pingTokens() map[string]string {
	if p := s.pingSwap.Load(); p != nil {
		return *p
	}
	return s.PingTokens
}

// SetSources switches to reloaded sources (tokens, kinds, limits).
func (s *Server) SetSources(src Sources) { s.swapped.Store(&src) }

// sources returns the current sources.
func (s *Server) sources() Sources {
	if p := s.swapped.Load(); p != nil {
		return *p
	}
	return s.Sources
}

// Handler serves the main listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /in/{source}", s.handleIn)
	s.mountCompat(mux)
	for _, m := range []string{"GET", "POST", "HEAD"} {
		mux.HandleFunc(m+" /ping/{name}", s.handlePing(false))
		mux.HandleFunc(m+" /ping/{name}/fail", s.handlePing(true))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.Ready != nil && !s.Ready() {
			http.Error(w, "targets not ready", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ready"))
	})
	for pattern, h := range s.Mounts {
		mux.Handle(pattern, h)
	}
	return mux
}

func (s *Server) handleIn(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("source")
	if !validSource.MatchString(name) {
		http.Error(w, "invalid source name", http.StatusBadRequest)
		return
	}
	src, _, ok := s.sources().Resolve(name)
	if !ok {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	s.receive(w, r, name, src, false, plainReply)
}

// reply writes the answer for a protocol: plain text for /in, JSON for the
// ntfy and Gotify compatible endpoints. code is the stellwerk status.
type reply func(w http.ResponseWriter, code int, detail string)

func plainReply(w http.ResponseWriter, code int, detail string) {
	if code >= 400 {
		http.Error(w, detail, code)
		return
	}
	w.WriteHeader(code)
}

// receive authenticates, limits, decodes and ingests one request. authed:
// the caller already checked the token (Gotify finds the source by it).
func (s *Server) receive(w http.ResponseWriter, r *http.Request, name string, src Source, authed bool, out reply) {
	if !authed && !sourceAuthorized(r, src) {
		out(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body []byte
	if src.Signature != nil {
		var err error
		if body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody)); err != nil {
			out(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
		if !src.Signature.Verify(r.Header, body) {
			out(w, http.StatusUnauthorized, "bad signature")
			return
		}
	}
	// Limit after the token check, so nobody without a token can use up a
	// sender's budget. One bucket per source name, also for "*" sources.
	if !s.withinRateLimit(w, name, src) {
		out(w, http.StatusTooManyRequests, "rate limit exceeded, retry later")
		return
	}
	if body == nil {
		var err error
		if body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody)); err != nil {
			out(w, http.StatusRequestEntityTooLarge, "body too large")
			return
		}
	}
	events, err := src.Decoder.Decode(source.Input{Source: name, Body: body, Query: r.URL.Query(), Header: r.Header})
	if err != nil {
		s.Ingest.Refuse(ports.RefusedRequest{Source: name, Kind: src.Kind, Reason: err.Error(), Raw: body})
		out(w, http.StatusBadRequest, err.Error())
		return
	}
	res := s.Ingest.Ingest(r.Context(), ports.IngestRequest{Events: events, Raw: body})
	out(w, statusFor(res.Outcome), res.Detail)
}

// sourceAuthorized checks the source's token; without one a signature is required.
func sourceAuthorized(r *http.Request, src Source) bool {
	if src.Token != "" {
		return checkToken(r, src.Token, src.TokenHeader)
	}
	return src.Signature != nil // never open by accident
}

// withinRateLimit takes from the source's bucket; when it is empty it
// records the refusal and sets Retry-After.
func (s *Server) withinRateLimit(w http.ResponseWriter, name string, src Source) bool {
	if src.RateLimit == nil {
		return true
	}
	s.limitOnce.Do(func() {
		if s.limits == nil {
			s.limits = newLimiter()
		}
	})
	ok, wait := s.limits.allow(name, *src.RateLimit)
	if ok {
		return true
	}
	if s.OnRateLimited != nil {
		s.OnRateLimited(name)
	}
	s.Ingest.Refuse(ports.RefusedRequest{Source: name, Kind: src.Kind, Reason: "rate limit exceeded"})
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	return false
}

// handlePing serves the healthchecks-style URLs. Unknown names and missing
// tokens look the same from outside, so names cannot be probed.
func (s *Server) handlePing(fail bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		token, ok := s.pingTokens()[name]
		if s.Pings == nil || !ok || !checkToken(r, token, "") {
			http.Error(w, "unknown heartbeat or wrong token", http.StatusNotFound)
			return
		}
		var err error
		if fail {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 10<<10))
			err = s.Pings.Fail(name, string(body))
		} else {
			err = s.Pings.Ping(name)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Write([]byte("ok"))
	}
}

// statusFor maps core outcomes to HTTP. Retryable failures are 5xx so
// Alertmanager and most senders try again, possibly on another node.
func statusFor(o ports.Outcome) int {
	switch o {
	case ports.Delivered:
		return http.StatusOK
	case ports.Accepted:
		return http.StatusAccepted
	case ports.NothingToDo:
		return http.StatusNoContent
	case ports.Failed:
		return http.StatusBadGateway
	case ports.TimedOut:
		return http.StatusGatewayTimeout
	case ports.Busy:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// checkToken accepts Bearer header, Basic auth password or ?token=, or
// only the given header if one is configured.
func checkToken(r *http.Request, want, header string) bool {
	got := ""
	if header != "" {
		got = r.Header.Get(header)
	} else if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if _, pw, ok := r.BasicAuth(); ok {
		got = pw
	} else {
		got = r.URL.Query().Get("token")
	}
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
