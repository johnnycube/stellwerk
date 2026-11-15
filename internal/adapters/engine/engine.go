// Package engine holds the decision engines that compile rule expressions
// (domain.Engine). An engine registers itself in init, the composition root
// imports internal/adapters/engine/all and hands every registered engine to
// the router.
//
// To add one (expr-lang, jq, JavaScript ...): create a package that
// implements domain.Engine, calls Register in init, import it in all, and
// run EngineContract from engine/enginetest against it.
package engine

import (
	"sort"
	"sync"

	"github.com/johnnycube/stellwerk/internal/core/domain"
)

var (
	mu      sync.RWMutex
	engines = map[string]domain.Engine{}
)

func Register(e domain.Engine) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := engines[e.Name()]; dup {
		panic("engine registered twice: " + e.Name())
	}
	engines[e.Name()] = e
}

// All returns the registered engines, sorted by name.
func All() []domain.Engine {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]domain.Engine, 0, len(engines))
	for _, e := range engines {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
