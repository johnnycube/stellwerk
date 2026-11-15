package http

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// The ntfy and Gotify compatible endpoints. An app that can notify ntfy or
// Gotify only needs its server URL changed:
//
//	ntfy:   https://stellwerk.example.org/ntfy     topic = a source of kind ntfy
//	Gotify: https://stellwerk.example.org/gotify   app token = a source of kind gotify
//
// Answers use the JSON those apps expect, and 200 for anything accepted.

func (s *Server) mountCompat(mux *http.ServeMux) {
	mux.HandleFunc("POST /ntfy/{topic}", s.handleNtfy)
	mux.HandleFunc("PUT /ntfy/{topic}", s.handleNtfy)
	for _, m := range []string{"GET", "POST", "PUT"} {
		for _, action := range []string{"publish", "send", "trigger"} {
			mux.HandleFunc(m+" /ntfy/{topic}/"+action, s.handleNtfy)
		}
	}
	mux.HandleFunc("POST /ntfy", s.handleNtfyJSON) // Uptime Kuma and others post JSON to the server URL itself
	mux.HandleFunc("POST /ntfy/{$}", s.handleNtfyJSON)
	mux.HandleFunc("PUT /ntfy/{$}", s.handleNtfyJSON)
	mux.HandleFunc("POST /gotify/message", s.handleGotify)
}

func (s *Server) handleNtfy(w http.ResponseWriter, r *http.Request) {
	s.ntfy(w, r, r.PathValue("topic"))
}

// handleNtfyJSON is the JSON publish to the root, the topic is in the body.
func (s *Server) handleNtfyJSON(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		ntfyReply("")(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	var p struct {
		Topic string `json:"topic"`
	}
	if json.Unmarshal(body, &p) != nil || p.Topic == "" {
		ntfyReply("")(w, http.StatusBadRequest, "JSON with a topic expected")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.ntfy(w, r, p.Topic)
}

func (s *Server) ntfy(w http.ResponseWriter, r *http.Request, topic string) {
	out := ntfyReply(topic)
	if !validSource.MatchString(topic) {
		out(w, http.StatusBadRequest, "invalid topic")
		return
	}
	src, _, ok := s.sources().Resolve(topic)
	if !ok || src.Kind != "ntfy" {
		out(w, http.StatusNotFound, "no source of kind ntfy with this name")
		return
	}
	// ntfy's ?auth= carries a whole Authorization header, base64 encoded.
	if a := r.URL.Query().Get("auth"); a != "" && r.Header.Get("Authorization") == "" {
		for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
			if v, err := enc.DecodeString(a); err == nil {
				r.Header.Set("Authorization", string(v))
				break
			}
		}
	}
	s.receive(w, r, topic, src, false, out)
}

func ntfyReply(topic string) reply {
	return func(w http.ResponseWriter, code int, detail string) {
		w.Header().Set("Content-Type", "application/json")
		if code >= 400 {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": code * 100, "http": code, "error": detail})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": shortID(), "time": time.Now().Unix(), "event": "message", "topic": topic})
	}
}

func (s *Server) handleGotify(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Gotify-Key")
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if h := r.Header.Get("Authorization"); token == "" && strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimPrefix(h, "Bearer ")
	}
	name, src, ok := "", Source{}, false
	for n, x := range s.sources() {
		if x.Kind == "gotify" && x.Token != "" && subtle.ConstantTimeCompare([]byte(x.Token), []byte(token)) == 1 {
			name, src, ok = n, x, true
		}
	}
	if !ok {
		gotifyReply(w, http.StatusUnauthorized, "you need to provide a valid access token")
		return
	}
	if name == "*" {
		name = "gotify"
	}
	s.receive(w, r, name, src, true, gotifyReply)
}

func gotifyReply(w http.ResponseWriter, code int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	if code >= 400 {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": http.StatusText(code), "errorCode": code, "errorDescription": detail})
		return
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<31))
	_ = json.NewEncoder(w).Encode(map[string]any{"id": n.Int64(), "appid": 0, "date": time.Now().UTC().Format(time.RFC3339)})
}

func shortID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
