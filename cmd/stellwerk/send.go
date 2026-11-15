package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// runSend posts one message to a running stellwerk, for scripts and cron:
//
//	stellwerk send -source backup -title "Backup done" "42 GB in 3 minutes"
//	echo "details" | stellwerk send -source backup -
//
// The source should be of kind generic. URL, source and token default to
// $STELLWERK_URL, $STELLWERK_SOURCE and $STELLWERK_TOKEN.
func runSend(args []string, stdin io.Reader, stdout io.Writer, environ []string) error {
	o, err := parseSend(args, stdout, envMap(environ))
	if err != nil {
		return err
	}
	if o.src == "" {
		return errors.New("send: -source is required (or $STELLWERK_SOURCE)")
	}
	text, err := sendText(o.args, stdin)
	if err != nil {
		return err
	}
	if text == "" && o.title == "" {
		return errors.New("send: give a message, or - to read it from stdin")
	}
	body := map[string]string{"message": text}
	if o.title != "" {
		body["title"] = o.title
	}
	if o.status != "" {
		body["status"] = o.status
	}
	raw, _ := json.Marshal(body)
	u, err := sendURL(o.base, o.src, o.labels)
	if err != nil {
		return err
	}
	return post(u, raw, o.token, o.timeout, stdout)
}

type sendOptions struct {
	base, src, token, title, status string
	labels                          multiFlag
	timeout                         time.Duration
	args                            []string
}

func envMap(environ []string) map[string]string {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	return env
}

func parseSend(args []string, stdout io.Writer, env map[string]string) (*sendOptions, error) {
	var o sendOptions
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stdout)
	fs.StringVar(&o.base, "url", or(env["STELLWERK_URL"], "http://127.0.0.1:8080"), "stellwerk base URL ($STELLWERK_URL)")
	fs.StringVar(&o.src, "source", env["STELLWERK_SOURCE"], "source name ($STELLWERK_SOURCE)")
	fs.StringVar(&o.token, "token", env["STELLWERK_TOKEN"], "the source's token ($STELLWERK_TOKEN)")
	fs.StringVar(&o.title, "title", "", "optional title")
	fs.StringVar(&o.status, "status", "", "optional status, e.g. failed (use status_field: status in the source options to route on it)")
	fs.Var(&o.labels, "label", "key=value, becomes a query parameter and so a label (repeatable)")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "request timeout")
	fs.Usage = func() {
		fmt.Fprintln(stdout, "usage: stellwerk send [flags] message|-")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	o.args = fs.Args()
	return &o, nil
}

// sendText is the message from the arguments, or from stdin for "-".
func sendText(args []string, stdin io.Reader) (string, error) {
	text := strings.Join(args, " ")
	if text != "-" {
		return text, nil
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func sendURL(base, src string, labels []string) (string, error) {
	u := strings.TrimRight(base, "/") + "/in/" + url.PathEscape(src)
	q := url.Values{}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return "", fmt.Errorf("send: -label %q: write key=value", l)
		}
		q.Set(k, v)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u, nil
}

func post(u string, raw []byte, token string, timeout time.Duration, stdout io.Writer) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("send: stellwerk answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	fmt.Fprintf(stdout, "%s\n", map[int]string{200: "delivered", 202: "accepted", 204: "nothing to do: no rule sends it, or it was dropped or silenced"}[resp.StatusCode])
	return nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
