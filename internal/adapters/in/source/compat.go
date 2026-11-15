package source

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v3"
)

// ntfy and Gotify are push services many apps can already talk to (the
// *arr apps, Uptime Kuma, Proxmox, Watchtower, Diun, Home Assistant ...).
// These kinds accept their publish format, so such an app only needs the
// server URL changed to stellwerk. The HTTP adapter serves them under
// /ntfy/<topic> and /gotify/message.

func init() {
	Register(Info{
		Kind:        "ntfy",
		Description: "The ntfy publish format: body as message, title, priority and tags from headers or query. Point an app's ntfy server URL at /ntfy, the topic is the source name.",
		Sample:      `Backup finished: 42 GB in 3 minutes`,
	}, func(*yaml.Node) (Decoder, error) { return Ntfy{}, nil })
	Register(Info{
		Kind:        "gotify",
		Description: "The Gotify message format: JSON or form with title, message and priority. Point an app's Gotify server URL at /gotify, the app token is the source's token.",
		Sample:      `{ "title": "Backup", "message": "42 GB in 3 minutes", "priority": 5 }`,
	}, func(*yaml.Node) (Decoder, error) { return Gotify{}, nil })
}

// Ntfy decodes ntfy publish requests: PUT/POST /<topic> with the message
// as body, a JSON publish {"topic", "message", ...}, or GET .../publish
// with ?message=. Labels: topic, priority (1 to 5), tags.
type Ntfy struct{}

var ntfyPriorities = map[string]int{"min": 1, "low": 2, "default": 3, "high": 4, "max": 5, "urgent": 5}

func (Ntfy) Decode(in Input) ([]*domain.Event, error) {
	pick := func(names ...string) string {
		for _, n := range names {
			if v := in.Header.Get(n); v != "" {
				return v
			}
			if v := in.Query.Get(strings.ToLower(n)); v != "" {
				return v
			}
		}
		return ""
	}
	d := map[string]any{"topic": in.Source}
	body := strings.TrimSpace(string(in.Body))
	if strings.HasPrefix(body, "{") && json.Valid(in.Body) { // JSON publish
		v, _ := DecodeJSON(in.Body)
		if m, ok := v.(map[string]any); ok {
			for k, x := range m {
				d[k] = x
			}
		}
	} else {
		d["message"] = body
		d["title"] = pick("X-Title", "Title", "ti", "t")
		d["priority"] = pick("X-Priority", "Priority", "prio", "p")
		if tags := pick("X-Tags", "Tags", "tag", "ta"); tags != "" {
			d["tags"] = splitTags(tags)
		}
	}
	if d["message"] == "" || d["message"] == nil {
		if m := pick("X-Message", "Message", "m"); m != "" {
			d["message"] = m
		} else if in.Query.Get("message") == "" && len(in.Body) == 0 {
			d["message"] = "triggered" // what ntfy sends for a bare /trigger
		}
	}
	prio := 3
	switch p := d["priority"].(type) {
	case string:
		if n, err := strconv.Atoi(p); err == nil && n >= 1 && n <= 5 {
			prio = n
		} else if n, ok := ntfyPriorities[strings.ToLower(p)]; ok {
			prio = n
		}
	case json.Number:
		if n, err := p.Int64(); err == nil && n >= 1 && n <= 5 {
			prio = int(n)
		}
	}
	d["priority"] = prio
	labels := map[string]string{"topic": in.Source, "priority": strconv.Itoa(prio)}
	if tags := tagList(d["tags"]); len(tags) > 0 {
		labels["tags"] = strings.Join(tags, ",")
	}
	if strings.TrimSpace(fmt.Sprint(d["title"])) == "" {
		delete(d, "title")
	}
	return []*domain.Event{{Source: in.Source, Kind: "ntfy", Labels: labels, Data: d}}, nil
}

func splitTags(s string) []any {
	var out []any
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func tagList(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, t := range l {
		out = append(out, fmt.Sprint(t))
	}
	return out
}

// Gotify decodes POST /message bodies, JSON or form encoded. Labels:
// priority (0 to 10, as sent).
type Gotify struct{}

func (Gotify) Decode(in Input) ([]*domain.Event, error) {
	d := map[string]any{}
	body := strings.TrimSpace(string(in.Body))
	switch {
	case strings.HasPrefix(body, "{"):
		v, err := DecodeJSON(in.Body)
		if err != nil {
			return nil, fmt.Errorf("gotify: %w", err)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gotify: expected a JSON object")
		}
		d = m
	case strings.HasPrefix(in.Header.Get("Content-Type"), "multipart/form-data"):
		return nil, fmt.Errorf("gotify: multipart forms are not supported, send JSON or url-encoded")
	default:
		form, err := url.ParseQuery(body)
		if err != nil {
			return nil, fmt.Errorf("gotify: %w", err)
		}
		for _, k := range []string{"title", "message", "priority"} {
			if v := form.Get(k); v != "" {
				d[k] = v
			}
		}
	}
	if msg, _ := d["message"].(string); strings.TrimSpace(msg) == "" {
		return nil, fmt.Errorf("gotify: message is required")
	}
	prio := 0
	switch p := d["priority"].(type) {
	case json.Number:
		n, _ := p.Int64()
		prio = int(n)
	case string:
		prio, _ = strconv.Atoi(p)
	}
	d["priority"] = prio
	return []*domain.Event{{Source: in.Source, Kind: "gotify", Labels: map[string]string{"priority": strconv.Itoa(prio)}, Data: d}}, nil
}
