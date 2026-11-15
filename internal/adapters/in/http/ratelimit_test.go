package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

func TestBucketRefills(t *testing.T) {
	l := newLimiter()
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	rl := RateLimit{Rate: 1, Burst: 2}
	if ok, _ := l.allow("a", rl); !ok {
		t.Fatal("1st")
	}
	if ok, _ := l.allow("a", rl); !ok {
		t.Fatal("2nd within burst")
	}
	ok, wait := l.allow("a", rl)
	if ok || wait != time.Second {
		t.Fatalf("3rd must wait 1s, got %v %v", ok, wait)
	}
	if ok, _ := l.allow("b", rl); !ok {
		t.Error("buckets are per source")
	}
	now = now.Add(1500 * time.Millisecond)
	if ok, _ := l.allow("a", rl); !ok {
		t.Error("refilled after 1.5s")
	}
	now = now.Add(time.Hour)
	l.allow("a", rl)
	l.allow("a", rl)
	if ok, _ := l.allow("a", rl); ok {
		t.Error("refill is capped at burst")
	}
}

type countingIngest struct {
	n       int
	refused []ports.RefusedRequest
}

func (c *countingIngest) Ingest(context.Context, ports.IngestRequest) ports.IngestResult {
	c.n++
	return ports.IngestResult{Outcome: ports.Delivered}
}

func (c *countingIngest) Refuse(r ports.RefusedRequest) { c.refused = append(c.refused, r) }

func TestServerRateLimitsAfterAuth(t *testing.T) {
	dec, _ := source.New("generic", nil)
	limited := &RateLimit{Rate: 0.001, Burst: 2}
	ing := &countingIngest{}
	var refused []string
	s := &Server{Ingest: ing, Sources: Sources{
		"busy":  {Kind: "generic", Token: "t", Decoder: dec, RateLimit: limited},
		"quiet": {Kind: "generic", Token: "t", Decoder: dec},
	}, OnRateLimited: func(src string) { refused = append(refused, src) }}
	h := s.Handler()
	do := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader("x"))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// requests without a token do not use up the budget
	for i := 0; i < 5; i++ {
		if rec := do("/in/busy", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated: %d", rec.Code)
		}
	}
	codes := []int{do("/in/busy", "t").Code, do("/in/busy", "t").Code}
	rec := do("/in/busy", "t")
	if codes[0] != 200 || codes[1] != 200 || rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("codes %v then %d, Retry-After %q", codes, rec.Code, rec.Header().Get("Retry-After"))
	}
	if ing.n != 2 || len(refused) != 1 || refused[0] != "busy" {
		t.Errorf("ingested %d, refused %v", ing.n, refused)
	}
	if len(ing.refused) != 1 || ing.refused[0].Reason != "rate limit exceeded" || ing.refused[0].Source != "busy" {
		t.Errorf("rate limited requests go to the audit log: %+v", ing.refused)
	}
	for i := 0; i < 10; i++ {
		if rec := do("/in/quiet", "t"); rec.Code != 200 {
			t.Fatalf("unlimited source got %d", rec.Code)
		}
	}
}

func TestBadPayloadIsRefusedIntoTheAuditLog(t *testing.T) {
	dec, _ := source.New("alertmanager", nil)
	ing := &countingIngest{}
	s := &Server{Ingest: ing, Sources: Sources{"am": {Kind: "alertmanager", Token: "t", Decoder: dec}}}
	req := httptest.NewRequest("POST", "/in/am", strings.NewReader(`{"nope":1}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || len(ing.refused) != 1 || string(ing.refused[0].Raw) != `{"nope":1}` || ing.refused[0].Kind != "alertmanager" {
		t.Errorf("code %d refused %+v", rec.Code, ing.refused)
	}
	// wrong tokens are not audited: anyone could flood the log
	req = httptest.NewRequest("POST", "/in/am", strings.NewReader(`x`))
	s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	if len(ing.refused) != 1 {
		t.Error("unauthorized requests must not reach the audit log")
	}
}
