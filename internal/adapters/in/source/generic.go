package source

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v4"
)

func init() {
	Register(Info{
		Kind:        "generic",
		Description: "Any JSON body, or plain text as {\"text\": ...}. Query parameters become labels.",
		Sample:      `{ "title": "Backup done", "message": "42 GB in 3 minutes" }`,
	}, func(opts *yaml.Node) (Decoder, error) {
		var o GenericOptions
		if err := decodeOptions(opts, &o); err != nil {
			return nil, fmt.Errorf("generic options: %w", err)
		}
		return &Generic{opts: o}, nil
	})
}

type GenericOptions struct {
	// Optional dot path whose value becomes the event status, e.g. "eventType".
	StatusField string `yaml:"status_field"`
	// Optional dot paths copied into labels, e.g. {host: "server.name"}.
	Labels map[string]string `yaml:"labels"`
}

type Generic struct{ opts GenericOptions }

func (g *Generic) Decode(in Input) ([]*domain.Event, error) {
	data, err := genericData(in.Body)
	if err != nil {
		return nil, err
	}
	labels := g.labels(in, data)
	ev := &domain.Event{Source: in.Source, Kind: "generic", Labels: labels, Data: data}
	if g.opts.StatusField != "" {
		if v, ok := domain.Lookup(data, g.opts.StatusField); ok && v != nil {
			ev.Status = fmt.Sprint(v)
		}
	}
	return []*domain.Event{ev}, nil
}

// genericData decodes a JSON body, or wraps any other body as {"text": ...}.
func genericData(body []byte) (any, error) {
	if json.Valid(body) {
		return DecodeJSON(body)
	}
	return map[string]any{"text": strings.TrimSpace(string(body))}, nil
}

// labels are the query parameters (except token) plus the configured paths.
func (g *Generic) labels(in Input, data any) map[string]string {
	labels := map[string]string{}
	for k, v := range in.Query {
		if k != "token" && len(v) > 0 {
			labels[k] = v[0]
		}
	}
	for label, p := range g.opts.Labels {
		if v, ok := domain.Lookup(data, p); ok && v != nil {
			labels[label] = fmt.Sprint(v)
		}
	}
	return labels
}
