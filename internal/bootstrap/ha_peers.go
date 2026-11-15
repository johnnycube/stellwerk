//go:build !noha

package bootstrap

import (
	"log/slog"
	"net/http"

	"github.com/johnnycube/stellwerk/internal/adapters/out/coordination/peers"
	"github.com/johnnycube/stellwerk/internal/config"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// HA support is compiled in by default. Build with -tags noha to leave it out.
func init() {
	haFactory = func(cfg *config.Config, log *slog.Logger) (ports.Coordinator, http.Handler) {
		s := peers.Settings{NodeID: cfg.NodeID, Secret: cfg.HA.Secret, Interval: cfg.HA.Interval}
		for _, p := range cfg.HA.Peers {
			s.Peers = append(s.Peers, peers.Peer{ID: p.ID, URL: p.URL})
		}
		p := peers.New(s, log)
		return p, p.Handler()
	}
}
