package source

import (
	"encoding/json"
	"fmt"
	"mime"
	"strings"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v3"
)

// CloudEvents (cloudevents.io, spec 1.0) over HTTP, in all three modes:
// structured (application/cloudevents+json), batch
// (application/cloudevents-batch+json) and binary (ce-* headers, the body
// is the data). It is the format to pick for anything new that should
// talk to stellwerk, and what Knative, Argo Events and many brokers send.
func init() {
	Register(Info{
		Kind:        "cloudevents",
		Description: "CloudEvents 1.0, structured, batch or binary mode. type, source, subject and extensions become labels.",
		Sample:      `{ "specversion": "1.0", "id": "b1", "source": "/backup/nas", "type": "org.example.backup.finished", "subject": "nightly", "data": { "message": "42 GB in 3 minutes" } }`,
	}, func(opts *yaml.Node) (Decoder, error) {
		var o CloudEventsOptions
		if err := decodeOptions(opts, &o); err != nil {
			return nil, fmt.Errorf("cloudevents options: %w", err)
		}
		return &CloudEvents{opts: o}, nil
	})
}

type CloudEventsOptions struct {
	// Optional dot path whose value becomes the event status, e.g.
	// "data.state". Without it, an extension attribute "status" is used.
	StatusField string `yaml:"status_field"`
}

type CloudEvents struct{ opts CloudEventsOptions }

// Context attributes that become labels. id is only used for dedupe.
var ceLabels = []string{"type", "source", "subject", "specversion", "datacontenttype", "dataschema", "time"}

func (c *CloudEvents) Decode(in Input) ([]*domain.Event, error) {
	ct, _, _ := mime.ParseMediaType(in.Header.Get("Content-Type"))
	switch {
	case ct == "application/cloudevents-batch+json":
		var batch []json.RawMessage
		if err := json.Unmarshal(in.Body, &batch); err != nil {
			return nil, fmt.Errorf("cloudevents batch: %w", err)
		}
		out := make([]*domain.Event, 0, len(batch))
		for i, raw := range batch {
			ev, err := c.structured(in.Source, raw)
			if err != nil {
				return nil, fmt.Errorf("cloudevents batch item %d: %w", i, err)
			}
			out = append(out, ev)
		}
		return out, nil
	case in.Header.Get("Ce-Specversion") != "":
		ev, err := c.binary(in)
		if err != nil {
			return nil, err
		}
		return []*domain.Event{ev}, nil
	default: // structured, also when the sender forgot the content type
		ev, err := c.structured(in.Source, in.Body)
		if err != nil {
			return nil, err
		}
		return []*domain.Event{ev}, nil
	}
}

func (c *CloudEvents) structured(source string, raw []byte) (*domain.Event, error) {
	v, err := DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("cloudevents: %w", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cloudevents: expected a JSON object")
	}
	if b64, ok := m["data_base64"].(string); ok && m["data"] == nil {
		m["data"] = b64 // kept as is, templates can show it
	}
	return c.event(source, m)
}

func (c *CloudEvents) binary(in Input) (*domain.Event, error) {
	m := map[string]any{}
	for k, v := range in.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "ce-") && len(v) > 0 {
			m[strings.TrimPrefix(lk, "ce-")] = v[0]
		}
	}
	if ct := in.Header.Get("Content-Type"); ct != "" {
		m["datacontenttype"] = ct
	}
	if len(in.Body) > 0 {
		if v, err := DecodeJSON(in.Body); err == nil && json.Valid(in.Body) {
			m["data"] = v
		} else {
			m["data"] = string(in.Body)
		}
	}
	return c.event(in.Source, m)
}

func (c *CloudEvents) event(source string, m map[string]any) (*domain.Event, error) {
	for _, req := range []string{"specversion", "id", "source", "type"} {
		if s, _ := m[req].(string); s == "" {
			return nil, fmt.Errorf("cloudevents: required attribute %q is missing", req)
		}
	}
	labels := map[string]string{}
	for _, k := range ceLabels {
		if s, ok := m[k].(string); ok && s != "" {
			labels[k] = s
		}
	}
	for k, v := range m { // extension attributes are scalars by spec
		switch k {
		case "id", "data", "data_base64":
			continue
		}
		if _, done := labels[k]; done {
			continue
		}
		switch v.(type) {
		case string, json.Number, bool:
			labels[k] = fmt.Sprint(v)
		}
	}
	ev := &domain.Event{Source: source, Kind: "cloudevents", Labels: labels, Data: m,
		Key: fmt.Sprint(m["source"]) + "#" + fmt.Sprint(m["id"])} // unique per the spec
	if c.opts.StatusField != "" {
		if v, ok := domain.Lookup(m, c.opts.StatusField); ok && v != nil {
			ev.Status = fmt.Sprint(v)
		}
	} else if s, ok := labels["status"]; ok {
		ev.Status = s
	}
	return ev, nil
}
