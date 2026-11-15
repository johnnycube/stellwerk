package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"
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
}
