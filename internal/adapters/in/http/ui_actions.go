package http

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// actionHeader must be set on every request that changes something. A
// browser cannot add it to a cross-site form post, so another web page
// cannot use your logged-in UI (CSRF), even with Basic auth cached.
const actionHeader = "X-Stellwerk-Action"

func (s *Server) action(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(actionHeader) == "" {
			http.Error(w, "missing "+actionHeader+" header", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleResend(w http.ResponseWriter, r *http.Request) {
	if s.Resend == nil {
		http.Error(w, "resending needs the audit log", http.StatusNotFound)
		return
	}
	id, err := s.Resend.Resend(r.PathValue("id"), r.URL.Query().Get("failed") == "true")
	if err != nil {
		http.Error(w, err.Error(), statusOf(err))
		return
	}
	writeJSON(w, map[string]string{"id": id})
}

func (s *Server) handleSilenceList(w http.ResponseWriter, _ *http.Request) {
	if s.Silences == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, s.Silences.Active())
}

type silenceRequest struct {
	Matchers string `json:"matchers"`
	For      string `json:"for"`
	Comment  string `json:"comment"`
}

func (s *Server) handleSilenceAdd(w http.ResponseWriter, r *http.Request) {
	if s.Silences == nil {
		http.Error(w, "silences are not available", http.StatusNotFound)
		return
	}
	var req silenceRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	by := "ui"
	if u, _, ok := r.BasicAuth(); ok && u != "" {
		by = "ui:" + u
	}
	sil, err := s.Silences.Silence(ports.SilenceRequest{Matchers: req.Matchers, For: req.For, Comment: req.Comment, By: by})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, sil)
}

func (s *Server) handleSilenceDelete(w http.ResponseWriter, r *http.Request) {
	if s.Silences == nil {
		http.Error(w, "silences are not available", http.StatusNotFound)
		return
	}
	if err := s.Silences.Expire(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), statusOf(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	if s.Reload == nil {
		http.Error(w, "reload is not available", http.StatusNotFound)
		return
	}
	if err := s.Reload(); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, map[string]string{"status": "reloaded"})
}

// statusOf maps "not found" errors to 404, everything else to 400. The
// adapter sees only ports, so it goes by the message.
func statusOf(err error) int {
	if strings.Contains(err.Error(), "not found") {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func (s *Server) handleCanary(w http.ResponseWriter, r *http.Request) {
	if s.Canary == nil {
		http.Error(w, "no canary configured, set canary.to", http.StatusNotFound)
		return
	}
	writeJSON(w, s.Canary.Run(r.Context()))
}
