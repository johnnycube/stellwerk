package domain

import (
	"fmt"
	"sort"
)

// Predicate is a compiled rule expression. Eval must be pure: no I/O, the
// same event always gives the same answer, and the cost is bounded. That
// keeps routing explainable, testable and safe to simulate.
type Predicate interface {
	Eval(ev *Event) (bool, error)
}

// Engine compiles rule expressions ("expr:" in a route). CEL is built in.
// Others (expr-lang, jq, JavaScript ...) plug in as adapters that implement
// this interface, see internal/adapters/engine.
//
// Anything slow or non-deterministic, such as asking an AI model, does not
// belong here. Such a step runs before routing and adds labels the rules can
// match on (see docs/adr/0002-pluggable-decision-engines.md).
type Engine interface {
	Name() string
	// Compile checks the expression once at startup. It must return an
	// error for anything that cannot evaluate to a bool.
	Compile(src string) (Predicate, error)
}

// Engines is the set of engines a router may use, plus the default for
// rules that do not name one.
type Engines struct {
	byName map[string]Engine
	def    string
}

// NewEngines returns the set. def must be one of them, unless no engine is
// given at all: then rules cannot use expressions.
func NewEngines(def string, engines ...Engine) (Engines, error) {
	e := Engines{byName: map[string]Engine{}, def: def}
	for _, en := range engines {
		if _, dup := e.byName[en.Name()]; dup {
			return Engines{}, fmt.Errorf("engine %q given twice", en.Name())
		}
		e.byName[en.Name()] = en
	}
	if len(engines) > 0 {
		if _, ok := e.byName[def]; !ok {
			return Engines{}, fmt.Errorf("default engine %q is not available (have %v)", def, e.Names())
		}
	}
	return e, nil
}

// Default is the engine used when a rule names none.
func (e Engines) Default() string { return e.def }

// Names lists the available engines, sorted.
func (e Engines) Names() []string {
	out := make([]string, 0, len(e.byName))
	for k := range e.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Compile compiles src with the named engine, or the default if name is empty.
func (e Engines) Compile(name, src string) (Predicate, error) {
	if name == "" {
		name = e.def
	}
	en, ok := e.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown expression engine %q (have %v)", name, e.Names())
	}
	return en.Compile(src)
}
