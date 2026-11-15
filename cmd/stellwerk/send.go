package main

import (
	"bytes"
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
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stdout)
	base := fs.String("url", or(env["STELLWERK_URL"], "http://127.0.0.1:8080"), "stellwerk base URL ($STELLWERK_URL)")
	src := fs.String("source", env["STELLWERK_SOURCE"], "source name ($STELLWERK_SOURCE)")
	token := fs.String("token", env["STELLWERK_TOKEN"], "the source's token ($STELLWERK_TOKEN)")
	title := fs.String("title", "", "optional title")
	status := fs.String("status", "", "optional status, e.g. failed (use status_field: status in the source options to route on it)")
	var labels multiFlag
	fs.Var(&labels, "label", "key=value, becomes a query parameter and so a label (repeatable)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	fs.Usage = func() {
		fmt.Fprintln(stdout, "usage: stellwerk send [flags] message|-")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *src == "" {
		return errors.New("send: -source is required (or $STELLWERK_SOURCE)")
	}
	text := strings.Join(fs.Args(), " ")
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
		if err != nil {
			return err
		}
		text = strings.TrimSpace(string(b))
	}
	if text == "" && *title == "" {
		return errors.New("send: give a message, or - to read it from stdin")
	}
	body := map[string]string{"message": text}
	if *title != "" {
		body["title"] = *title
	}
	if *status != "" {
		body["status"] = *status
	}
	raw, _ := json.Marshal(body)
	u := strings.TrimRight(*base, "/") + "/in/" + url.PathEscape(*src)
	q := url.Values{}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return fmt.Errorf("send: -label %q: write key=value", l)
		}
		q.Set(k, v)
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	resp, err := (&http.Client{Timeout: *timeout}).Do(req)
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
