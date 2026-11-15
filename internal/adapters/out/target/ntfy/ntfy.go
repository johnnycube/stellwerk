// Package ntfy is a push target for ntfy (ntfy.sh or self-hosted). It works
// without Matrix and without your own infrastructure, which makes it a good
// fallback: a phone push when the homeserver is down.
package ntfy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v3"
)

func init() {
	target.Register(target.Info{
		Type:        "ntfy",
		Description: "Publishes every message as a push notification to an ntfy topic.",
		Address:     "the topic",
	}, func(_ context.Context, env target.Env, n *yaml.Node) (target.Target, error) {
		var s Settings
		if err := target.DecodeSettings(n, &s); err != nil {
			return nil, err
		}
		return New(env, s)
	})
}

type Settings struct {
	URL      string        `yaml:"url"`      // server, default https://ntfy.sh
	Token    string        `yaml:"token"`    // access token (tk_...)
	Username string        `yaml:"username"` // or basic auth
	Password string        `yaml:"password"`
	Tags     []string      `yaml:"tags"` // emoji shortcodes or tags added to every message
	Timeout  time.Duration `yaml:"timeout"`
}

type Ntfy struct {
	s      Settings
	client *http.Client
}

func New(env target.Env, s Settings) (*Ntfy, error) {
	if s.URL == "" {
		s.URL = "https://ntfy.sh"
	}
	if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
		return nil, fmt.Errorf("ntfy %s/%s: url must start with http:// or https://", env.Target, env.Account)
	}
	if s.Token != "" && s.Username != "" {
		return nil, fmt.Errorf("ntfy %s/%s: set token or username, not both", env.Target, env.Account)
	}
	if s.Timeout == 0 {
		s.Timeout = 10 * time.Second
	}
	s.URL = strings.TrimRight(s.URL, "/")
	return &Ntfy{s: s, client: &http.Client{Timeout: s.Timeout}}, nil
}

// Publish is the JSON body ntfy accepts on its root URL.
type Publish struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title,omitempty"`
	Message  string   `json:"message"`
	Priority int      `json:"priority,omitempty"` // 1 min ... 5 max
	Tags     []string `json:"tags,omitempty"`
	Markdown bool     `json:"markdown"`
}

var priorities = map[string]int{domain.PriorityLow: 2, domain.PriorityNormal: 3, domain.PriorityHigh: 4, domain.PriorityUrgent: 5}

func (n *Ntfy) Send(ctx context.Context, topic string, msg domain.Message) error {
	if strings.TrimSpace(topic) == "" {
		return fmt.Errorf("ntfy: destination address (topic) is empty")
	}
	title, body := split(msg.Markdown)
	p := Publish{Topic: topic, Title: title, Message: body, Priority: priorities[msg.Priority], Tags: n.s.Tags, Markdown: true}
	if p.Priority == 0 && msg.Loud {
		p.Priority = 4
	}
	raw, _ := json.Marshal(p)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.s.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case n.s.Token != "":
		req.Header.Set("Authorization", "Bearer "+n.s.Token)
	case n.s.Username != "":
		req.SetBasicAuth(n.s.Username, n.s.Password)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return target.StatusError("ntfy", resp)
	}
	return nil
}

// split uses the first line as title when there is more, so the push shows
// a headline and the details below it.
func split(md string) (title, body string) {
	md = strings.TrimSpace(md)
	first, rest, found := strings.Cut(md, "\n")
	if !found || strings.TrimSpace(rest) == "" {
		return "", md
	}
	return target.Title(first), strings.TrimSpace(rest)
}
