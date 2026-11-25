package source

import (
	"net/http"
	"strings"
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v4"
)

func ce(t *testing.T, opts string) Decoder {
	t.Helper()
	var n yaml.Node
	if opts != "" {
		_ = yaml.Unmarshal([]byte(opts), &n)
		n = *n.Content[0]
	}
	d, err := New("cloudevents", &n)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const ceSample = `{"specversion":"1.0","id":"b1","source":"/backup/nas","type":"org.example.backup.finished","subject":"nightly",
 "time":"2026-11-15T03:00:00Z","severity":"warning","attempt":3,"datacontenttype":"application/json","data":{"message":"42 GB","state":"ok"}}`

func TestCloudEventsStructured(t *testing.T) {
	evs, err := ce(t, "").Decode(Input{
		Source: "nas", Body: []byte(ceSample),
		Header: http.Header{"Content-Type": {"application/cloudevents+json; charset=utf-8"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	if ev.Kind != "cloudevents" || ev.Source != "nas" || ev.Key != "/backup/nas#b1" {
		t.Errorf("%+v", ev)
	}
	for k, want := range map[string]string{
		"type": "org.example.backup.finished", "source": "/backup/nas", "subject": "nightly",
		"severity": "warning", "attempt": "3", "specversion": "1.0", "time": "2026-11-15T03:00:00Z",
	} {
		if ev.Labels[k] != want {
			t.Errorf("label %s=%q, want %q", k, ev.Labels[k], want)
		}
	}
	if _, ok := ev.Labels["id"]; ok {
		t.Error("id is not a label")
	}
	if v, _ := domain.Lookup(ev.Data, "data.message"); v != "42 GB" {
		t.Errorf("data %v", ev.Data)
	}
	rend, _ := domain.NewRenderer(nil)
	out, _ := rend.Render("", ev)
	if out != "**nas** org.example.backup.finished · nightly\n42 GB" {
		t.Errorf("template: %q", out)
	}
}

func TestCloudEventsStatus(t *testing.T) {
	evs, _ := ce(t, "status_field: data.state").Decode(Input{Body: []byte(ceSample), Header: http.Header{}})
	if evs[0].Status != "ok" {
		t.Errorf("status_field: %q", evs[0].Status)
	}
	body := strings.Replace(ceSample, `"severity":"warning"`, `"status":"firing"`, 1)
	evs, _ = ce(t, "").Decode(Input{Body: []byte(body), Header: http.Header{}})
	if evs[0].Status != "firing" {
		t.Errorf("status extension: %q", evs[0].Status)
	}
}

func TestCloudEventsBatchAndBinary(t *testing.T) {
	batch := "[" + ceSample + "," + strings.Replace(ceSample, `"id":"b1"`, `"id":"b2"`, 1) + "]"
	evs, err := ce(t, "").Decode(Input{Body: []byte(batch), Header: http.Header{"Content-Type": {"application/cloudevents-batch+json"}}})
	if err != nil || len(evs) != 2 || evs[1].Key != "/backup/nas#b2" {
		t.Fatalf("%v %+v", err, evs)
	}
	h := http.Header{}
	h.Set("Ce-Specversion", "1.0")
	h.Set("Ce-Id", "x1")
	h.Set("Ce-Source", "/ci")
	h.Set("Ce-Type", "build.failed")
	h.Set("Ce-Branch", "main")
	h.Set("Content-Type", "application/json")
	evs, err = ce(t, "").Decode(Input{Source: "ci", Body: []byte(`{"text":"red"}`), Header: h})
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	if ev.Labels["type"] != "build.failed" || ev.Labels["branch"] != "main" || ev.Labels["datacontenttype"] != "application/json" || ev.Key != "/ci#x1" {
		t.Errorf("%+v", ev)
	}
	if v, _ := domain.Lookup(ev.Data, "data.text"); v != "red" {
		t.Errorf("%v", ev.Data)
	}
	h.Set("Content-Type", "text/plain")
	evs, _ = ce(t, "").Decode(Input{Body: []byte("plain words"), Header: h})
	if v, _ := domain.Lookup(evs[0].Data, "data"); v != "plain words" {
		t.Errorf("%v", evs[0].Data)
	}
}

func TestCloudEventsErrors(t *testing.T) {
	d := ce(t, "")
	for name, in := range map[string]Input{
		"not json":       {Body: []byte("nope"), Header: http.Header{}},
		"not an object":  {Body: []byte("[1]"), Header: http.Header{}},
		"missing type":   {Body: []byte(`{"specversion":"1.0","id":"1","source":"/x"}`), Header: http.Header{}},
		"bad batch":      {Body: []byte(`{}`), Header: http.Header{"Content-Type": {"application/cloudevents-batch+json"}}},
		"bad batch item": {Body: []byte(`[{"id":"1"}]`), Header: http.Header{"Content-Type": {"application/cloudevents-batch+json"}}},
		"binary missing": {Body: nil, Header: http.Header{"Ce-Specversion": {"1.0"}}},
	} {
		if _, err := d.Decode(in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	var bad yaml.Node
	_ = yaml.Unmarshal([]byte("status_field: [1]"), &bad)
	if _, err := New("cloudevents", bad.Content[0]); err == nil {
		t.Error("bad options")
	}
	b64 := `{"specversion":"1.0","id":"1","source":"/x","type":"t","data_base64":"aGk="}`
	evs, err := d.Decode(Input{Body: []byte(b64), Header: http.Header{}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := domain.Lookup(evs[0].Data, "data"); v != "aGk=" {
		t.Errorf("%v", evs[0].Data)
	}
}
