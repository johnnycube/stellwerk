package scheduler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// SelfPing tells an outside service that stellwerk is alive, e.g.
// healthchecks.io or Uptime Kuma's push monitor. If stellwerk dies, the
// outside service notices the silence and alerts through its own channel:
// the one alarm stellwerk cannot raise about itself.
//
// While stellwerk is up but cannot deliver (an account not ready, a
// breaker open), it calls FailURL instead, with the reason in the body.
type SelfPing struct {
	URL     string
	FailURL string // default URL + "/fail" (the healthchecks.io convention)
	Every   time.Duration
	Health  func() (ok bool, problem string)
	Client  *http.Client
	Log     *slog.Logger
}

func (p *SelfPing) Run(ctx context.Context) {
	if p.Client == nil {
		p.Client = &http.Client{Timeout: 10 * time.Second}
	}
	p.ping(ctx)
	t := time.NewTicker(p.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.ping(ctx)
		}
	}
}

func (p *SelfPing) ping(ctx context.Context) {
	url, body := p.URL, ""
	if ok, problem := p.Health(); !ok {
		url, body = p.FailURL, problem
		if url == "" {
			url = strings.TrimRight(p.URL, "/") + "/fail"
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		p.Log.Error("self ping: bad url", "err", err)
		return
	}
	req.Header.Set("User-Agent", "stellwerk")
	resp, err := p.Client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			p.Log.Warn("self ping failed", "err", err)
		}
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		p.Log.Warn("self ping answered", "status", resp.Status)
	}
}
