package source

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/domain"
)

func TestNtfyDecoder(t *testing.T) {
	d, _ := New("ntfy", nil)
	h := http.Header{}
	h.Set("X-Title", "Backup")
	h.Set("Priority", "high")
	h.Set("Tags", "warning, disk")
	evs, err := d.Decode(Input{Source: "nas", Body: []byte("42 GB done\n"), Header: h, Query: url.Values{}})
	if err != nil {
		t.Fatal(err)
	}
	ev := evs[0]
	if ev.Kind != "ntfy" || ev.Labels["priority"] != "4" || ev.Labels["tags"] != "warning,disk" || ev.Labels["topic"] != "nas" {
		t.Errorf("%+v", ev)
	}
	rend, _ := domain.NewRenderer(nil)
	if out, _ := rend.Render("", ev); out != "**Backup**\n42 GB done" {
		t.Errorf("%q", out)
	}
	for name, in := range map[string]struct {
		in   Input
		prio string
		msg  string
	}{
		"query":   {Input{Source: "x", Query: url.Values{"t": {"T"}, "p": {"5"}, "message": {"from query"}}, Header: http.Header{}}, "5", "from query"},
		"json":    {Input{Source: "x", Body: []byte(`{"topic":"x","message":"json body","priority":2,"tags":["a"]}`), Header: http.Header{}, Query: url.Values{}}, "2", "json body"},
		"trigger": {Input{Source: "x", Header: http.Header{}, Query: url.Values{}}, "3", "triggered"},
		"bad":     {Input{Source: "x", Body: []byte("m"), Header: http.Header{"Priority": {"9"}}, Query: url.Values{}}, "3", "m"},
		"header":  {Input{Source: "x", Header: http.Header{"X-Message": {"hdr"}}, Query: url.Values{}}, "3", "hdr"},
	} {
		evs, _ := d.Decode(in.in)
		if evs[0].Labels["priority"] != in.prio || evs[0].Data.(map[string]any)["message"] != in.msg {
			t.Errorf("%s: %+v", name, evs[0])
		}
	}
}

func TestGotifyDecoder(t *testing.T) {
	d, _ := New("gotify", nil)
	evs, err := d.Decode(Input{Source: "app", Body: []byte(`{"title":"T","message":"M","priority":8}`), Header: http.Header{}})
	if err != nil || evs[0].Labels["priority"] != "8" || evs[0].Kind != "gotify" {
		t.Fatalf("%v %+v", err, evs)
	}
	evs, err = d.Decode(Input{Source: "app", Body: []byte(`title=T&message=hello+world&priority=2`), Header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}})
	if err != nil || evs[0].Data.(map[string]any)["message"] != "hello world" || evs[0].Labels["priority"] != "2" {
		t.Fatalf("%v %+v", err, evs)
	}
	rend, _ := domain.NewRenderer(nil)
	if out, _ := rend.Render("", evs[0]); out != "**T**\nhello world" {
		t.Errorf("%q", out)
	}
	for name, body := range map[string]string{"no message": `{"title":"x"}`, "bad json": `{`, "array": `[1]`, "bad form": `%zz`} {
		if _, err := d.Decode(Input{Body: []byte(body), Header: http.Header{}}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := d.Decode(Input{Body: []byte("x"), Header: http.Header{"Content-Type": {"multipart/form-data; boundary=x"}}}); err == nil {
		t.Error("multipart")
	}
}
