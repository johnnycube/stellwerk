package source

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v3"
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
	var data any
	if json.Valid(in.Body) {
		var err error
		if data, err = DecodeJSON(in.Body); err != nil {
			return nil, err
		}
	} else {
		data = map[string]any{"text": strings.TrimSpace(string(in.Body))}
	}
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
	ev := &domain.Event{Source: in.Source, Kind: "generic", Labels: labels, Data: data}
	if g.opts.StatusField != "" {
		if v, ok := domain.Lookup(data, g.opts.StatusField); ok && v != nil {
			ev.Status = fmt.Sprint(v)
		}
	}
	return []*domain.Event{ev}, nil
}
