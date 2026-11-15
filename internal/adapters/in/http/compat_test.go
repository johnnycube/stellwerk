package http

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
)

func compatServer() (*Server, *countingIngest) {
	ntfy, _ := source.New("ntfy", nil)
	gotify, _ := source.New("gotify", nil)
	generic, _ := source.New("generic", nil)
	ing := &countingIngest{}
	return &Server{Ingest: ing, Sources: Sources{
		"uptime":  {Kind: "ntfy", Token: "tk_up", Decoder: ntfy},
		"proxmox": {Kind: "gotify", Token: "AppTok", Decoder: gotify},
		"other":   {Kind: "generic", Token: "AppTok2", Decoder: generic},
	}}, ing
}

func TestNtfyCompat(t *testing.T) {
	s, ing := compatServer()
	h := s.Handler()
	do := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := do("POST", "/ntfy/uptime", "Server down", map[string]string{"Authorization": "Bearer tk_up", "Title": "Uptime"})
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp["event"] != "message" || resp["topic"] != "uptime" || resp["id"] == "" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	auth := base64.RawURLEncoding.EncodeToString([]byte("Bearer tk_up"))
	for name, c := range map[string]struct {
		method, path, body string
		hdr                map[string]string
		code               int
	}{
		"put":             {"PUT", "/ntfy/uptime", "x", map[string]string{"Authorization": "Bearer tk_up"}, 200},
		"basic password":  {"POST", "/ntfy/uptime", "x", map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("any:tk_up"))}, 200},
		"auth param":      {"GET", "/ntfy/uptime/trigger?auth=" + auth, "", nil, 200},
		"publish message": {"GET", "/ntfy/uptime/publish?message=hi&auth=" + auth, "", nil, 200},
		"json root":       {"POST", "/ntfy/", `{"topic":"uptime","message":"j"}`, map[string]string{"Authorization": "Bearer tk_up"}, 200},
		"json no slash":   {"POST", "/ntfy", `{"topic":"uptime","message":"j","title":"Up","priority":5}`, map[string]string{"Authorization": "Bearer tk_up"}, 200},
		"json no topic":   {"POST", "/ntfy/", `{"message":"j"}`, nil, 400},
		"wrong token":     {"POST", "/ntfy/uptime", "x", map[string]string{"Authorization": "Bearer nope"}, 401},
		"not ntfy kind":   {"POST", "/ntfy/other", "x", map[string]string{"Authorization": "Bearer AppTok2"}, 404},
		"unknown topic":   {"POST", "/ntfy/nope", "x", nil, 404},
		"bad topic":       {"POST", "/ntfy/a%20b", "x", nil, 400},
	} {
		if rec := do(c.method, c.path, c.body, c.hdr); rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		} else if c.code >= 400 && !strings.Contains(rec.Body.String(), `"http":`) {
			t.Errorf("%s: ntfy style error expected: %s", name, rec.Body)
		}
	}
	if ing.n != 7 {
		t.Errorf("ingested %d", ing.n)
	}
}

func TestGotifyCompat(t *testing.T) {
	s, ing := compatServer()
	h := s.Handler()
	do := func(path string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"title":"Backup","message":"done","priority":5}`))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("/gotify/message", map[string]string{"X-Gotify-Key": "AppTok"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"date"`) {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if rec := do("/gotify/message?token=AppTok", nil); rec.Code != 200 {
		t.Errorf("query token %d", rec.Code)
	}
	if rec := do("/gotify/message", map[string]string{"Authorization": "Bearer AppTok"}); rec.Code != 200 {
		t.Errorf("bearer %d", rec.Code)
	}
	if rec := do("/gotify/message", map[string]string{"X-Gotify-Key": "AppTok2"}); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "errorDescription") {
		t.Errorf("a generic source's token is not a Gotify token: %d", rec.Code)
	}
	if ing.n != 3 {
		t.Errorf("ingested %d", ing.n)
	}
}
