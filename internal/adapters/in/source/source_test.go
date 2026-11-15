package source

import (
	"net/url"
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v3"
)

func opts(t *testing.T, s string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(s), &n); err != nil {
		t.Fatal(err)
	}
	return n.Content[0]
}

const twoAlerts = `{"version":"4","status":"firing","receiver":"r",
 "groupLabels":{"alertname":"X"},"commonLabels":{"alertname":"X","namespace":"prod"},
 "alerts":[{"status":"firing","labels":{"alertname":"X","severity":"critical"},"fingerprint":"f1"},
           {"status":"resolved","labels":{"alertname":"X","severity":"warning"},"fingerprint":"f2"}]}`

func TestAlertmanagerGrouped(t *testing.T) {
	d, _ := New("alertmanager", nil)
	evs, err := d.Decode(Input{Source: "am", Body: []byte(twoAlerts)})
	if err != nil || len(evs) != 1 {
		t.Fatalf("evs=%d err=%v", len(evs), err)
	}
	ev := evs[0]
	if ev.Status != "firing" || ev.Labels["namespace"] != "prod" || ev.Labels["receiver"] != "r" || ev.Kind != "alertmanager" {
		t.Errorf("event: %+v", ev)
	}
	if v, _ := domain.Lookup(ev.Data, "alerts.1.status"); v != "resolved" {
		t.Errorf("data not JSON-like: %v", v)
	}
}

func TestAlertmanagerSplit(t *testing.T) {
	d, err := New("alertmanager", opts(t, "split_alerts: true"))
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := d.Decode(Input{Source: "am", Body: []byte(twoAlerts)})
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	if evs[0].Labels["severity"] != "critical" || evs[1].Status != "resolved" || evs[1].Key != "am:f2:resolved" {
		t.Errorf("split: %+v / %+v", evs[0], evs[1])
	}
	if alerts, _ := domain.Lookup(evs[1].Data, "alerts"); len(alerts.([]any)) != 1 {
		t.Errorf("split payload should hold one alert")
	}
}

func TestAlertmanagerRejectsGarbage(t *testing.T) {
	d, _ := New("alertmanager", nil)
	if _, err := d.Decode(Input{Body: []byte(`{"hello":1}`)}); err == nil {
		t.Error("expected error without status")
	}
}

func TestGeneric(t *testing.T) {
	d, _ := New("generic", opts(t, "status_field: eventType\nlabels: { show: series.title }"))
	evs, err := d.Decode(Input{
		Source: "sonarr", Body: []byte(`{"eventType":"Download","series":{"title":"Severance"}}`),
		Query: url.Values{"host": {"nas"}, "token": {"secret"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	if ev.Status != "Download" || ev.Labels["show"] != "Severance" || ev.Labels["host"] != "nas" || ev.Labels["token"] != "" {
		t.Errorf("event: %+v", ev)
	}
	evs, _ = d.Decode(Input{Body: []byte("disk full")})
	if v, _ := domain.Lookup(evs[0].Data, "text"); v != "disk full" {
		t.Errorf("plain text: %v", v)
	}
}

func TestUnknownKind(t *testing.T) {
	if _, err := New("nope", nil); err == nil {
		t.Error("expected error")
	}
	if _, err := New("alertmanager", opts(t, "split_alerts: maybe")); err == nil {
		t.Error("expected options type error")
	}
}
