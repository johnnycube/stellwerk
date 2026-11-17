package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"text/template/parse"
	"time"
	"unicode/utf8"
)

const maxMessageLen = 12000

// BuiltinTemplates are used when a route names no template: the template
// named like the event kind if it exists, otherwise "generic".
// Data is always JSON-like (maps), so field names are as in the payload.
var BuiltinTemplates = map[string]string{
	"alertmanager": `{{ if eq .Status "firing" }}🔥{{ else }}✅{{ end }} **{{ or (index .Labels "alertname") "Alert" }}** {{ upper .Status }}{{ with index .Labels "severity" }} · {{ . }}{{ end }}{{ with index .Labels "namespace" }} · ns/{{ . }}{{ end }}
{{ range get .Data "alerts" }}
- {{ $resolved := eq (get . "status") "resolved" }}{{ if $resolved }}~~{{ end }}{{ or (get . "annotations.summary") (get . "labels.alertname") }}{{ if $resolved }}~~{{ end }}{{ with get . "annotations.description" }}
  {{ . }}{{ end }}{{ with get . "generatorURL" }} [source]({{ . }}){{ end }}{{ end }}`,

	"cloudevents": `**{{ .Source }}** {{ index .Labels "type" }}{{ with index .Labels "subject" }} · {{ . }}{{ end }}
{{ $msg := or (get .Data "data.message") (get .Data "data.text") (get .Data "data.summary") }}{{ if $msg }}{{ $msg }}{{ else }}{{ with get .Data "data" }}` + "```json\n{{ json . }}\n```" + `{{ end }}{{ end }}`,

	"ntfy": `{{ with get .Data "title" }}**{{ . }}**
{{ end }}{{ get .Data "message" }}`,

	"gotify": `{{ with get .Data "title" }}**{{ . }}**
{{ end }}{{ get .Data "message" }}`,

	"generic": `{{ $title := or (get .Data "title") (get .Data "subject") (get .Data "eventType") }}{{ $msg := or (get .Data "message") (get .Data "text") (get .Data "body") }}**{{ .Source }}**{{ with $title }}: {{ . }}{{ end }}
{{ if $msg }}{{ $msg }}{{ else }}` + "```json\n{{ json .Data }}\n```" + `{{ end }}`,
}

type Renderer struct {
	tmpls   map[string]*template.Template
	sources map[string]string
	custom  map[string]bool
}

func NewRenderer(custom map[string]string) (*Renderer, error) {
	r := &Renderer{tmpls: map[string]*template.Template{}, sources: map[string]string{}, custom: map[string]bool{}}
	all := map[string]string{}
	for k, v := range BuiltinTemplates {
		all[k] = v
	}
	for k, v := range custom {
		all[k] = v
		r.custom[k] = true
	}
	for name, src := range all {
		t, err := template.New(name).Funcs(funcs).Option("missingkey=zero").Parse(src)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		if err := checkPatterns(t.Root); err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		r.tmpls[name] = t
		r.sources[name] = src
	}
	return r, nil
}

func (r *Renderer) Has(name string) bool { _, ok := r.tmpls[name]; return ok }

// Sources returns template name -> (source, builtin).
func (r *Renderer) Sources() map[string]struct {
	Source  string
	Builtin bool
} {
	out := map[string]struct {
		Source  string
		Builtin bool
	}{}
	for k, v := range r.sources {
		out[k] = struct {
			Source  string
			Builtin bool
		}{v, !r.custom[k]}
	}
	return out
}

func (r *Renderer) Render(name string, ev *Event) (string, error) {
	if name == "" {
		name = "generic"
		if r.Has(ev.Kind) {
			name = ev.Kind
		}
	}
	t, ok := r.tmpls[name]
	if !ok {
		return "", fmt.Errorf("unknown template %q", name)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ev); err != nil {
		return "", err
	}
	return Truncate(strings.TrimSpace(buf.String()), maxMessageLen), nil
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "\n\n… (truncated)"
}

var funcs = template.FuncMap{
	"get":   func(data any, p string) any { v, _ := Lookup(data, p); return v },
	"json":  func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return Truncate(string(b), 3000) },
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
	"join":  func(sep string, v []string) string { return strings.Join(v, sep) },
	"default": func(def, v any) any {
		if v == nil || v == "" {
			return def
		}
		return v
	},
	"since": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	// Text helpers take the value last, so they work in pipelines:
	// {{ get .Data "message" | firstLine | trunc 80 }}.
	"firstLine": func(v any) string { s, _, _ := strings.Cut(text(v), "\n"); return strings.TrimSpace(s) },
	"lines":     func(v any) []string { return strings.Split(strings.ReplaceAll(text(v), "\r\n", "\n"), "\n") },
	"trim":      func(v any) string { return strings.TrimSpace(text(v)) },
	"trunc":     trunc,
	"replace":   func(from, to string, v any) string { return strings.ReplaceAll(text(v), from, to) },
	"regexFind": regexFind,
}

// text turns a template value into a string; a missing value is "".
func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}

// trunc cuts to n characters (runes, not bytes) and marks the cut with "…".
func trunc(n int, v any) string {
	s := text(v)
	if n < 1 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

var regexCache sync.Map // pattern -> *regexp.Regexp

// regexFind returns the first match of re in the value, or the first
// capture group if re has one, e.g. {{ get .Data "title" | regexFind "reason: (\\w+)" }}.
// No match is "". An invalid pattern fails the render.
func regexFind(re string, v any) (string, error) {
	c, ok := regexCache.Load(re)
	if !ok {
		x, err := regexp.Compile(re)
		if err != nil {
			return "", fmt.Errorf("regexFind: %w", err)
		}
		c, _ = regexCache.LoadOrStore(re, x)
	}
	m := c.(*regexp.Regexp).FindStringSubmatch(text(v))
	switch {
	case m == nil:
		return "", nil
	case len(m) > 1:
		return m[1], nil
	default:
		return m[0], nil
	}
}

// checkPatterns compiles every literal regexFind pattern in a template, so a
// bad pattern fails at load (and in -check) instead of at the first message.
func checkPatterns(n parse.Node) error {
	if c, ok := n.(*parse.CommandNode); ok {
		if err := checkRegexFind(c); err != nil {
			return err
		}
	}
	for _, c := range children(n) {
		if err := checkPatterns(c); err != nil {
			return err
		}
	}
	return nil
}

func checkRegexFind(c *parse.CommandNode) error {
	if len(c.Args) < 2 {
		return nil
	}
	id, ok := c.Args[0].(*parse.IdentifierNode)
	re, lit := c.Args[1].(*parse.StringNode)
	if !ok || !lit || id.Ident != "regexFind" {
		return nil
	}
	if _, err := regexp.Compile(re.Text); err != nil {
		return fmt.Errorf("regexFind %q: %w", re.Text, err)
	}
	return nil
}

// children lists the nodes below n that can hold a command.
func children(n parse.Node) []parse.Node {
	var out []parse.Node
	add := func(ns ...parse.Node) {
		for _, c := range ns {
			if c != nil && !reflectNil(c) {
				out = append(out, c)
			}
		}
	}
	switch n := n.(type) {
	case *parse.ListNode:
		add(n.Nodes...)
	case *parse.ActionNode:
		add(n.Pipe)
	case *parse.PipeNode:
		for _, c := range n.Cmds {
			add(c)
		}
	case *parse.CommandNode:
		add(n.Args...)
	case *parse.IfNode:
		add(n.Pipe, n.List, n.ElseList)
	case *parse.RangeNode:
		add(n.Pipe, n.List, n.ElseList)
	case *parse.WithNode:
		add(n.Pipe, n.List, n.ElseList)
	case *parse.TemplateNode:
		add(n.Pipe)
	}
	return out
}

// reflectNil catches typed nil pointers (an absent else branch is a nil
// *ListNode inside a non-nil Node interface).
func reflectNil(n parse.Node) bool {
	switch n := n.(type) {
	case *parse.ListNode:
		return n == nil
	case *parse.PipeNode:
		return n == nil
	}
	return false
}
