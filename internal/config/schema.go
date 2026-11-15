package config

import (
	_ "embed"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

//go:embed config.go
var configSource string

// Schema returns a JSON Schema for the config file, for editors (the YAML
// language server) and CI. Descriptions come from the comments in
// config.go. kinds and types are the registered source kinds and target
// types, they become enums.
func Schema(kinds, types []string) ([]byte, error) {
	g := &schemaGen{docs: fieldDocs(), defs: map[string]any{}}
	root := g.object(reflect.TypeOf(Config{}))
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = "https://github.com/johnnycube/stellwerk/docs/stellwerk.schema.json"
	root["title"] = "stellwerk configuration"
	root["$defs"] = g.defs
	setEnum(g.defs, "Source", "kind", kinds)
	setEnum(g.defs, "Target", "type", types)
	setEnum(g.defs, "Delivery", "mode", []string{"sync", "async"})
	setEnum(g.defs, "Log", "level", []string{"debug", "info", "warn", "error"})
	setEnum(g.defs, "Log", "format", []string{"text", "json"})
	setEnum(g.defs, "Route", "priority", []string{"low", "normal", "high", "urgent"})
	setEnum(g.defs, "TestExpect", "priority", []string{"low", "normal", "high", "urgent"})
	setEnum(g.defs, "Signature", "preset", []string{"github", "gitea", "forgejo"})
	return json.MarshalIndent(root, "", "  ")
}

func setEnum(defs map[string]any, def, field string, values []string) {
	if len(values) == 0 {
		return
	}
	if d, ok := defs[def].(map[string]any); ok {
		if p, ok := d["properties"].(map[string]any)[field].(map[string]any); ok {
			p["enum"] = values
		}
	}
}

type schemaGen struct {
	docs map[string]string // Type.Field -> comment
	defs map[string]any
}

var (
	durationType   = reflect.TypeOf(time.Duration(0))
	nodeType       = reflect.TypeOf(yaml.Node{})
	stringListType = reflect.TypeOf(StringList{})
)

func (g *schemaGen) schema(t reflect.Type) map[string]any {
	switch t {
	case durationType:
		return map[string]any{"type": "string", "pattern": `^(0|-?([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+)$`, "examples": []string{"30s", "10m", "24h"}}
	case nodeType:
		return map[string]any{} // free form, depends on the type or kind
	case stringListType:
		return map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return g.schema(t.Elem())
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schema(t.Elem())}
	case reflect.Struct:
		name := t.Name()
		if _, done := g.defs[name]; !done {
			g.defs[name] = nil // break cycles
			g.defs[name] = g.object(t)
		}
		return map[string]any{"$ref": "#/$defs/" + name}
	default: // kinds the config does not use
		return map[string]any{}
	}
}

func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		s := g.schema(f.Type)
		if d := g.docs[t.Name()+"."+f.Name]; d != "" {
			if _, isRef := s["$ref"]; isRef {
				s = map[string]any{"allOf": []any{s}, "description": d}
			} else {
				s["description"] = d
			}
		}
		props[tag] = s
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if d := g.docs[t.Name()]; d != "" {
		out["description"] = d
	}
	return out
}

// fieldDocs reads the comments of config.go: on types and on fields,
// above or behind them.
func fieldDocs() map[string]string {
	out := map[string]string{}
	f, err := parser.ParseFile(token.NewFileSet(), "config.go", configSource, parser.ParseComments)
	if err != nil {
		return out
	}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if gd.Doc != nil {
				out[ts.Name.Name] = clean(gd.Doc.Text())
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, fld := range st.Fields.List {
				var doc []string
				if fld.Doc != nil {
					doc = append(doc, clean(fld.Doc.Text()))
				}
				if fld.Comment != nil {
					doc = append(doc, clean(fld.Comment.Text()))
				}
				for _, n := range fld.Names {
					if len(doc) > 0 {
						out[ts.Name.Name+"."+n.Name] = strings.Join(doc, " ")
					}
				}
			}
		}
	}
	return out
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }
