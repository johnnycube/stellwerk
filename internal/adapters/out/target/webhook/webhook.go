// Package webhook is an outgoing HTTP target: it POSTs each message as JSON.
// Useful to chain other systems (n8n, Home Assistant, a pager) next to Matrix.
// Accounts can carry different URLs or credentials.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"go.yaml.in/yaml/v4"
)

func init() {
	target.Register(target.Info{
		Type:        "webhook",
		Description: "POSTs every message as JSON to a URL.",
		Address:     "optional path appended to the base URL",
	}, func(_ context.Context, env target.Env, n *yaml.Node) (target.Target, error) {
		var s Settings
		if err := target.DecodeSettings(n, &s); err != nil {
			return nil, err
		}
		if s.URL == "" {
			return nil, fmt.Errorf("webhook %s/%s: url is required", env.Target, env.Account)
		}
		if s.Timeout == 0 {
			s.Timeout = 10 * time.Second
		}
		return &Webhook{s: s, node: env.NodeID, account: env.Account, client: &http.Client{Timeout: s.Timeout}}, nil
	})
}

type Settings struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Timeout time.Duration     `yaml:"timeout"`
}

type Webhook struct {
	s       Settings
	node    string
	account string
	client  *http.Client
}

// Payload is the JSON body sent to the receiver.
type Payload struct {
	Source   string    `json:"source"`
	Route    string    `json:"route"`
	Markdown string    `json:"markdown"`
	Loud     bool      `json:"loud"`
	Mentions []string  `json:"mentions,omitempty"`
	Account  string    `json:"account"`
	Node     string    `json:"node"`
	Time     time.Time `json:"time"`
}

func (w *Webhook) Send(ctx context.Context, address string, msg domain.Message) error {
	body, _ := json.Marshal(Payload{Source: msg.Source, Route: msg.Route, Markdown: msg.Markdown, Loud: msg.Loud, Mentions: msg.Mentions, Account: w.account, Node: w.node, Time: time.Now().UTC()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.s.URL+address, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "stellwerk")
	for k, v := range w.s.Headers {
		req.Header.Set(k, v)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return target.StatusError("webhook", resp)
	}
	return nil
}
