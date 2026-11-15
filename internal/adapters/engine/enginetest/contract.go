// Package enginetest is the contract every decision engine must pass.
package enginetest

import (
	"encoding/json"
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/domain"
)

// Expressions are written in the engine's own language. All of them are
// evaluated against Event below.
type Expressions struct {
	True         string   // must be true for Event
	False        string   // must be false for Event
	RuntimeError string   // compiles, but fails on Event (e.g. a missing key)
	Invalid      []string // must not compile, include one that is not boolean
	Data         string   // true for Event, reads nested data incl. a list
}

// Event is what the expressions see: source "am", status "firing",
// label env=prod and data {"items": [{"n": 1}, {"n": 3}]}.
func Event() *domain.Event {
	return &domain.Event{Source: "am", Status: "firing", Labels: map[string]string{"env": "prod"},
		Data: map[string]any{"items": []any{map[string]any{"n": json.Number("1")}, map[string]any{"n": json.Number("3")}}}}
}

func EngineContract(t *testing.T, e domain.Engine, x Expressions) {
	t.Helper()
	if e.Name() == "" {
		t.Fatal("engine needs a name")
	}
	eval := func(src string) (bool, error) {
		t.Helper()
		p, err := e.Compile(src)
		if err != nil {
			t.Fatalf("compile %q: %v", src, err)
		}
		return p.Eval(Event())
	}
	t.Run("true", func(t *testing.T) {
		if ok, err := eval(x.True); !ok || err != nil {
			t.Errorf("%q: %v %v", x.True, ok, err)
		}
	})
	t.Run("false", func(t *testing.T) {
		if ok, err := eval(x.False); ok || err != nil {
			t.Errorf("%q: %v %v", x.False, ok, err)
		}
	})
	t.Run("nested data", func(t *testing.T) {
		if ok, err := eval(x.Data); !ok || err != nil {
			t.Errorf("%q: %v %v", x.Data, ok, err)
		}
	})
	t.Run("runtime error is reported, not a match", func(t *testing.T) {
		if ok, err := eval(x.RuntimeError); ok || err == nil {
			t.Errorf("%q: %v %v", x.RuntimeError, ok, err)
		}
	})
	t.Run("invalid expressions fail at compile time", func(t *testing.T) {
		for _, src := range x.Invalid {
			if _, err := e.Compile(src); err == nil {
				t.Errorf("%q compiled", src)
			}
		}
	})
	t.Run("pure: same event, same answer, event untouched", func(t *testing.T) {
		p, _ := e.Compile(x.True)
		ev := Event()
		for range 3 {
			if ok, _ := p.Eval(ev); !ok {
				t.Error("answer changed")
			}
		}
		if ev.Labels["env"] != "prod" || len(ev.Labels) != 1 {
			t.Error("event was modified")
		}
	})
	t.Run("nil labels and data", func(t *testing.T) {
		p, _ := e.Compile(x.False)
		if _, err := p.Eval(&domain.Event{}); err != nil {
			t.Errorf("empty event: %v", err)
		}
	})
}
