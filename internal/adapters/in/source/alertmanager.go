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
		Kind:        "alertmanager",
		Description: "Prometheus Alertmanager webhook (version 4). Labels are the group's common labels plus status.",
		Sample:      alertmanagerSample,
	}, func(opts *yaml.Node) (Decoder, error) {
		var o AlertmanagerOptions
		if err := decodeOptions(opts, &o); err != nil {
			return nil, fmt.Errorf("alertmanager options: %w", err)
		}
		return &Alertmanager{opts: o}, nil
	})
}

type AlertmanagerOptions struct {
	// One event per alert instead of one per group. Labels are then the
	// alert's own labels, so routes can differ per alert.
	SplitAlerts bool `yaml:"split_alerts"`
}

type Alertmanager struct{ opts AlertmanagerOptions }

type amPayload struct {
	Version      string            `json:"version"`
	GroupKey     string            `json:"groupKey"`
	Status       string            `json:"status"`
	Receiver     string            `json:"receiver"`
	GroupLabels  map[string]string `json:"groupLabels"`
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Fingerprint string            `json:"fingerprint"`
	} `json:"alerts"`
}

func (a *Alertmanager) Decode(in Input) ([]*domain.Event, error) {
	var p amPayload
	if err := json.Unmarshal(in.Body, &p); err != nil {
		return nil, fmt.Errorf("invalid alertmanager payload: %w", err)
	}
	if p.Status == "" {
		return nil, fmt.Errorf("invalid alertmanager payload: status missing")
	}
	data, err := DecodeJSON(in.Body)
	if err != nil {
		return nil, err
	}
	if !a.opts.SplitAlerts || len(p.Alerts) <= 1 {
		labels := map[string]string{}
		for k, v := range p.CommonLabels {
			labels[k] = v
		}
		for k, v := range p.GroupLabels {
			labels[k] = v
		}
		labels["receiver"] = p.Receiver
		return []*domain.Event{{Source: in.Source, Kind: "alertmanager", Status: p.Status, Labels: labels, Data: data}}, nil
	}
	// Split: each event gets a payload with just its alert, so the
	// alertmanager template and field matchers keep working.
	root, _ := data.(map[string]any)
	alerts, _ := root["alerts"].([]any)
	var out []*domain.Event
	for i, al := range p.Alerts {
		single := map[string]any{}
		for k, v := range root {
			single[k] = v
		}
		single["alerts"] = []any{alerts[i]}
		single["status"] = al.Status
		labels := map[string]string{"receiver": p.Receiver}
		for k, v := range al.Labels {
			labels[k] = v
		}
		key := ""
		if al.Fingerprint != "" {
			key = strings.Join([]string{"am", al.Fingerprint, al.Status}, ":")
		}
		out = append(out, &domain.Event{Source: in.Source, Kind: "alertmanager", Status: al.Status, Labels: labels, Data: single, Key: key})
	}
	return out, nil
}

const alertmanagerSample = `{
  "version": "4",
  "status": "firing",
  "receiver": "stellwerk",
  "groupLabels": { "alertname": "KubePodCrashLooping" },
  "commonLabels": { "alertname": "KubePodCrashLooping", "severity": "critical", "namespace": "media" },
  "alerts": [{
    "status": "firing",
    "labels": { "alertname": "KubePodCrashLooping", "pod": "sonarr-0" },
    "annotations": { "summary": "Pod media/sonarr-0 is crash looping", "description": "Restarted 5 times in 10 minutes" },
    "generatorURL": "http://prometheus/graph",
    "fingerprint": "a1b2c3"
  }]
}`
