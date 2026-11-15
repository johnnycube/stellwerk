package cel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/johnnycube/stellwerk/internal/adapters/engine"
	"github.com/johnnycube/stellwerk/internal/adapters/engine/enginetest"
	"github.com/johnnycube/stellwerk/internal/core/domain"
)

func jsonData(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEngineContract(t *testing.T) {
	enginetest.EngineContract(t, New(), enginetest.Expressions{
		True: `labels.env == "prod" && source == "am"`, False: `status == "resolved"`,
		RuntimeError: `data.missing.key == "x"`, Invalid: []string{`source ==`, `1 + 1`},
		Data: `size(data.items) == 2 && data.items[1].n == 3`,
	})
}

func TestRegistered(t *testing.T) {
	for _, e := range engine.All() {
		if e.Name() == Name {
			return
		}
	}
	t.Error("cel is not registered")
}

func TestCEL(t *testing.T) {
	ev := &domain.Event{Source: "sonarr", Status: "firing", Labels: map[string]string{"severity": "critical"},
		Data: jsonData(t, `{"eventType":"Download","series":{"title":"Severance"},"episodes":[{"n":2},{"n":3}],"size":1.5}`)}
	cases := map[string]bool{
		`labels.severity in ["critical", "page"] && status == "firing"`: true,
		`size(data.episodes) == 2 && data.episodes[1].n == 3`:           true,
		`data.series.title.lowerAscii().startsWith("sev")`:              true,
		`data.size > 1`:                                   true,
		`data.?movie.title.orValue("") == ""`:             true,
		`has(data.eventType) && data.eventType == "Test"`: false,
		`data.movie.title == "x"`:                         false, // missing key = no match
	}
	for expr, want := range cases {
		m, err := Compile(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got, _ := m.Eval(ev); got != want {
			t.Errorf("%s: got %v want %v", expr, got, want)
		}
	}
	for _, bad := range []string{`source`, `source ==`, `nope == 1`} {
		if _, err := Compile(bad); err == nil {
			t.Errorf("expected compile error for %q", bad)
		}
	}
}

func TestCELNumbers(t *testing.T) {
	ev := &domain.Event{Data: map[string]any{"i": json.Number("12"), "f": json.Number("1.5"), "big": float64(3), "frac": 2.5,
		"list": []any{json.Number("1")}}}
	for expr, want := range map[string]bool{
		`data.i == 12 && data.i > 11`:       true,
		`data.f > 1.4 && data.f < 1.6`:      true,
		`data.big == 3`:                     true,
		`data.frac > 2`:                     true,
		`data.list[0] == 1`:                 true,
		`source == "" && size(labels) == 0`: true,
	} {
		m, err := Compile(expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if got, err := m.Eval(ev); err != nil || got != want {
			t.Errorf("%s: %v %v", expr, got, err)
		}
	}
}
