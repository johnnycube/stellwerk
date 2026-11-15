package source

import (
	"net/http"
	"net/url"
	"testing"
)

// The decoders read untrusted bodies from the internet. They may refuse
// anything, but must never panic, and an event they return must be usable.
func fuzzDecoder(f *testing.F, kind string, seeds ...string) {
	for _, s := range seeds {
		f.Add([]byte(s), "")
	}
	f.Fuzz(func(t *testing.T, body []byte, ct string) {
		d, err := New(kind, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := http.Header{}
		if ct != "" {
			h.Set("Content-Type", ct)
		}
		evs, err := d.Decode(Input{Source: "fuzz", Body: body, Header: h, Query: url.Values{"title": {"x"}}})
		if err != nil {
			return
		}
		for _, ev := range evs {
			if ev == nil || ev.Source != "fuzz" || ev.Kind == "" {
				t.Fatalf("bad event %+v", ev)
			}
		}
	})
}

func FuzzAlertmanager(f *testing.F) {
	fuzzDecoder(f, "alertmanager", `{"version":"4","status":"firing","commonLabels":{"a":"b"},"alerts":[{"status":"firing","labels":{"x":"y"}},{"status":"resolved"}]}`, `{"status":"firing","alerts":[{}]}`, `{}`)
}

func FuzzGeneric(f *testing.F) {
	fuzzDecoder(f, "generic", `{"title":"x"}`, `plain`, `[1,2]`, `{"a":{"b":[null]}}`)
}

func FuzzCloudEvents(f *testing.F) {
	fuzzDecoder(f, "cloudevents", ceSample, `[`+ceSample+`]`, `{"specversion":"1.0"}`)
}

func FuzzNtfy(f *testing.F) {
	fuzzDecoder(f, "ntfy", `hello`, `{"topic":"x","message":"m","priority":9}`, ``)
}

func FuzzGotify(f *testing.F) {
	fuzzDecoder(f, "gotify", `{"message":"m","priority":"x"}`, `message=a&priority=b`, `{"message":1}`)
}
