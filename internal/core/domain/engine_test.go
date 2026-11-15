package domain

import (
	"errors"
	"testing"
)

// testEngine understands three expressions: "true", "false" and "error"
// (fails at evaluation). Everything else does not compile. It shows that the
// router depends on the Engine interface only.
type testEngine struct{ name string }

func (e testEngine) Name() string { return e.name }

func (e testEngine) Compile(src string) (Predicate, error) {
	switch src {
	case "true", "false", "error", "labels.x":
		return testPredicate(src), nil
	}
	return nil, errors.New("test engine: cannot compile " + src)
}

type testPredicate string

func (p testPredicate) Eval(ev *Event) (bool, error) {
	switch p {
	case "error":
		return false, errors.New("boom")
	case "labels.x":
		return ev.Labels["x"] == "1", nil
	}
	return p == "true", nil
}

func testEngines(t *testing.T) Engines {
	t.Helper()
	e, err := NewEngines("test", testEngine{"test"}, testEngine{"other"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEngines(t *testing.T) {
	e := testEngines(t)
	if e.Default() != "test" || len(e.Names()) != 2 || e.Names()[0] != "other" {
		t.Errorf("%v %v", e.Default(), e.Names())
	}
	if _, err := e.Compile("", "true"); err != nil {
		t.Error(err)
	}
	if _, err := e.Compile("other", "false"); err != nil {
		t.Error(err)
	}
	if _, err := e.Compile("nope", "true"); err == nil {
		t.Error("unknown engine must fail")
	}
	if _, err := NewEngines("missing", testEngine{"test"}); err == nil {
		t.Error("default must exist")
	}
	if _, err := NewEngines("test", testEngine{"test"}, testEngine{"test"}); err == nil {
		t.Error("duplicate must fail")
	}
	none, err := NewEngines("cel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := none.Compile("", "true"); err == nil {
		t.Error("no engines: expressions must fail")
	}
}

func TestRouterUsesTheRulesEngine(t *testing.T) {
	r := testRouter(t,
		Rule{Name: "x", Expr: "labels.x", Engine: "other", To: []string{"a"}},
		Rule{Name: "never", Expr: "false", To: []string{"b"}},
	)
	res, _ := r.Route(&Event{Labels: map[string]string{"x": "1"}})
	if len(res.Deliveries) != 1 || res.Deliveries[0].Route != "x" {
		t.Errorf("%+v", res)
	}
	res, _ = r.Route(&Event{})
	if len(res.Deliveries) != 0 || res.Trace[0].Reason != "expr is false" {
		t.Errorf("%+v", res.Trace)
	}
}
