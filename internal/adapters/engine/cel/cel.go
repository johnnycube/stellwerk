// Package cel is the built-in decision engine: Google's Common Expression
// Language. It is fast, side-effect free and bounded by a cost limit, which
// is what domain.Engine asks for.
package cel

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"

	"github.com/johnnycube/stellwerk/internal/adapters/engine"
	"github.com/johnnycube/stellwerk/internal/core/domain"
)

// Name is the engine name used in config ("engine: cel").
const Name = "cel"

func init() { engine.Register(New()) }

// Engine implements domain.Engine.
type Engine struct{}

func New() Engine { return Engine{} }

func (Engine) Name() string { return Name }

func (Engine) Compile(src string) (domain.Predicate, error) { return Compile(src) }

// CEL variables available in route "expr:":
//
//	source  string               URL source, e.g. "alertmanager", "sonarr"
//	status  string               "firing" / "resolved" (Alertmanager), "" otherwise
//	labels  map(string, string)  Alertmanager common+group labels, or query params
//	data    dyn                  JSON body (Alertmanager payload as JSON object)
//
// Examples:
//
//	labels.severity in ["critical", "page"] && status == "firing"
//	has(data.eventType) && data.eventType == "Test"
//	size(data.alerts) >= 5
//	data.?series.title.orValue("").startsWith("Severance")
//	labels.namespace.matches("^(prod|media)-")
var celEnv = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("source", cel.StringType),
		cel.Variable("status", cel.StringType),
		cel.Variable("labels", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("data", cel.DynType),
		cel.OptionalTypes(),                   // data.?a.b.orValue(x)
		cel.CrossTypeNumericComparisons(true), // data.count > 3 works for ints and doubles
		ext.Strings(),                         // lowerAscii, split, replace, ...
	)
})

const celCostLimit = 100_000

// Predicate is a compiled CEL expression.
type Predicate struct {
	src string
	prg cel.Program
}

// Compile type-checks a CEL expression, it must return bool.
func Compile(src string) (*Predicate, error) {
	env, err := celEnv()
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, fmt.Errorf("cel: %w", iss.Err())
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("cel: expression must return bool, got %s", ast.OutputType())
	}
	prg, err := env.Program(ast, cel.CostLimit(celCostLimit), cel.EvalOptions(cel.OptOptimize))
	if err != nil {
		return nil, fmt.Errorf("cel: %w", err)
	}
	return &Predicate{src: src, prg: prg}, nil
}

func (c *Predicate) Eval(ev *domain.Event) (bool, error) {
	labels := ev.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	out, _, err := c.prg.Eval(map[string]any{
		"source": ev.Source,
		"status": ev.Status,
		"labels": labels,
		"data":   celData(ev.Data),
	})
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("cel: non-bool result %v", out.Value())
	}
	return b, nil
}

// celData turns decoded JSON into CEL-friendly values: integral numbers become
// int64 so size(), indexes and int literals behave as expected.
func celData(data any) any { return normalizeNumbers(data) }

func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeNumbers(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeNumbers(val)
		}
		return out
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	default:
		return v
	}
}
