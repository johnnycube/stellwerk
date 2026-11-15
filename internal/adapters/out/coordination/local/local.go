// Package local is the default coordinator: one node, everything in memory.
package local

import (
	"context"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/out/coordination/memstate"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

type Local struct {
	id string
	st *memstate.State
}

var _ ports.Coordinator = (*Local)(nil)

func New(nodeID string) *Local { return &Local{id: nodeID, st: memstate.New()} }

func (l *Local) NodeID() string                           { return l.id }
func (l *Local) IsLeader() bool                           { return true }
func (l *Local) SetHeartbeat(name string, at time.Time)   { l.st.SetHeartbeat(name, at) }
func (l *Local) Heartbeat(name string) (time.Time, bool)  { return l.st.Heartbeat(name) }
func (l *Local) Seen(key string) bool                     { return l.st.Seen(key) }
func (l *Local) Claim(key string, ttl time.Duration) bool { return l.st.Claim(key, ttl) }
func (l *Local) Run(ctx context.Context)                  { <-ctx.Done() }
func (l *Local) Status() ports.ClusterStatus {
	return ports.ClusterStatus{Enabled: false, NodeID: l.id, Leader: l.id}
}
