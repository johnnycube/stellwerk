package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
)

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestSignatureVerify(t *testing.T) {
	gh := Signature{Header: "X-Hub-Signature-256", Prefix: "sha256=", Secret: "s3cret"}
	body := []byte(`{"action":"opened"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+sign("s3cret", string(body)))
	if !gh.Verify(h, body) {
		t.Error("valid signature")
	}
	if gh.Verify(h, []byte(`{"action":"closed"}`)) {
		t.Error("changed body must fail")
	}
	h.Set("X-Hub-Signature-256", sign("s3cret", string(body)))
	if gh.Verify(h, body) {
		t.Error("missing prefix must fail")
	}
	h.Set("X-Hub-Signature-256", "sha256=zz")
	if gh.Verify(h, body) {
		t.Error("not hex must fail")
	}
	if (Signature{Header: "X", Secret: ""}).Verify(http.Header{"X": {sign("", "")}}, nil) {
		t.Error("an empty secret never verifies")
	}
}

func TestServerSignatureAndTokenHeader(t *testing.T) {
	dec, _ := source.New("generic", nil)
	ing := &countingIngest{}
	s := &Server{Ingest: ing, Sources: Sources{
		"forgejo": {Kind: "generic", Decoder: dec, Signature: &Signature{Header: "X-Forgejo-Signature", Secret: "k"}},
		"both":    {Kind: "generic", Token: "t", Decoder: dec, Signature: &Signature{Header: "X-Sig", Secret: "k"}},
		"gitlab":  {Kind: "generic", Token: "glt", TokenHeader: "X-Gitlab-Token", Decoder: dec},
		"open":    {Kind: "generic", Decoder: dec}, // misconfigured: must stay closed
	}}
	h := s.Handler()
	do := func(path, body string, hdr map[string]string) int {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	body := `{"title":"push"}`
	for name, c := range map[string]struct {
		path string
		hdr  map[string]string
		want int
	}{
		"signed":                {"/in/forgejo", map[string]string{"X-Forgejo-Signature": sign("k", body)}, 200},
		"wrong signature":       {"/in/forgejo", map[string]string{"X-Forgejo-Signature": sign("other", body)}, 401},
		"no signature":          {"/in/forgejo", nil, 401},
		"both ok":               {"/in/both", map[string]string{"Authorization": "Bearer t", "X-Sig": sign("k", body)}, 200},
		"both, token missing":   {"/in/both", map[string]string{"X-Sig": sign("k", body)}, 401},
		"both, signature wrong": {"/in/both", map[string]string{"Authorization": "Bearer t", "X-Sig": "00"}, 401},
		"token header":          {"/in/gitlab", map[string]string{"X-Gitlab-Token": "glt"}, 200},
		"bearer ignored":        {"/in/gitlab", map[string]string{"Authorization": "Bearer glt"}, 401},
		"no credentials at all": {"/in/open", nil, 401},
	} {
		if got := do(c.path, body, c.hdr); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	if ing.n != 3 {
		t.Errorf("ingested %d", ing.n)
	}
	big := strings.Repeat("x", maxBody+1)
	if got := do("/in/forgejo", big, map[string]string{"X-Forgejo-Signature": sign("k", big)}); got != http.StatusRequestEntityTooLarge {
		t.Errorf("too large: %d", got)
	}
}
