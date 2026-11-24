// Package email is an SMTP target. It is the classic independent second
// channel: when the Matrix homeserver is down, a destination can fall back
// to mail. The message goes out as plain text (the Markdown is readable as
// is), the subject is the first line without Markdown marks.
package email

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"go.yaml.in/yaml/v4"
)

func init() {
	target.Register(target.Info{
		Type:        "email",
		Description: "Sends every message as a plain text mail over SMTP.",
		Address:     "recipients, comma separated, e.g. me@example.org",
	}, func(_ context.Context, env target.Env, n *yaml.Node) (target.Target, error) {
		var s Settings
		if err := target.DecodeSettings(n, &s); err != nil {
			return nil, err
		}
		return New(env, s)
	})
}

type Settings struct {
	Host          string        `yaml:"host"`
	Port          int           `yaml:"port"`     // default 587, 465 for tls: implicit
	TLS           string        `yaml:"tls"`      // starttls (default), implicit, none
	Username      string        `yaml:"username"` // empty: no AUTH
	Password      string        `yaml:"password"`
	From          string        `yaml:"from"`           // e.g. "stellwerk <alerts@example.org>"
	SubjectPrefix string        `yaml:"subject_prefix"` // default "[stellwerk] "
	Timeout       time.Duration `yaml:"timeout"`        // whole conversation, default 20s
	// InsecureSkipVerify is for test servers only.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

type Email struct {
	s    Settings
	from *mail.Address
	node string
}

func New(env target.Env, s Settings) (*Email, error) {
	where := env.Target + "/" + env.Account
	if s.Host == "" || s.From == "" {
		return nil, fmt.Errorf("email %s: host and from are required", where)
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return nil, fmt.Errorf("email %s: from: %w", where, err)
	}
	switch s.TLS {
	case "":
		s.TLS = "starttls"
	case "starttls", "implicit", "none":
	default:
		return nil, fmt.Errorf("email %s: tls must be starttls, implicit or none", where)
	}
	if s.Port == 0 {
		s.Port = 587
		if s.TLS == "implicit" {
			s.Port = 465
		}
	}
	if s.Timeout == 0 {
		s.Timeout = 20 * time.Second
	}
	if s.SubjectPrefix == "" {
		s.SubjectPrefix = "[stellwerk] "
	}
	return &Email{s: s, from: from, node: env.NodeID}, nil
}

// Send delivers one mail to every recipient in address.
func (e *Email) Send(ctx context.Context, address string, msg domain.Message) error {
	return classify(e.send(ctx, address, msg))
}

// classify: SMTP 5xx replies (no such user, relay denied, auth failed) are
// permanent, 4xx (greylisting, mailbox busy) and network errors are not.
func classify(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return ports.Permanent(err)
	}
	return err
}

func (e *Email) send(ctx context.Context, address string, msg domain.Message) error {
	to, err := recipients(address)
	if err != nil {
		return ports.Permanent(err)
	}
	deadline := e.deadline(ctx)
	tlsCfg := &tls.Config{ServerName: e.s.Host, InsecureSkipVerify: e.s.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // opt-in setting for relays with self-signed certificates
	conn, err := e.dial(ctx, deadline, tlsCfg)
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c, err := smtp.NewClient(conn, e.s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	if err := e.handshake(c, tlsCfg); err != nil {
		return err
	}
	return e.transmit(c, to, msg)
}

// deadline is the earlier of the configured timeout and ctx's deadline.
func (e *Email) deadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(e.s.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}

func (e *Email) dial(ctx context.Context, deadline time.Time, tlsCfg *tls.Config) (net.Conn, error) {
	addr := net.JoinHostPort(e.s.Host, strconv.Itoa(e.s.Port))
	dialer := &net.Dialer{Deadline: deadline}
	if e.s.TLS == "implicit" {
		return (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	}
	return dialer.DialContext(ctx, "tcp", addr)
}

// handshake upgrades to TLS and authenticates, as configured.
func (e *Email) handshake(c *smtp.Client, tlsCfg *tls.Config) error {
	if e.s.TLS == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp: server does not offer STARTTLS (set tls: none only for a trusted local relay)")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if e.s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.s.Username, e.s.Password, e.s.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	return nil
}

// transmit sends the envelope and the mail, then quits.
func (e *Email) transmit(c *smtp.Client, to []*mail.Address, msg domain.Message) error {
	if err := c.Mail(e.from.Address); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	for _, r := range to {
		if err := c.Rcpt(r.Address); err != nil {
			return fmt.Errorf("smtp to %s: %w", r.Address, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(e.compose(to, msg)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return c.Quit()
}

func recipients(address string) ([]*mail.Address, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("email: destination address (recipients) is empty")
	}
	l, err := mail.ParseAddressList(address)
	if err != nil {
		return nil, fmt.Errorf("email: recipients %q: %w", address, err)
	}
	return l, nil
}

func (e *Email) compose(to []*mail.Address, msg domain.Message) []byte {
	var b bytes.Buffer
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	addrs := make([]string, len(to))
	for i, a := range to {
		addrs[i] = a.String()
	}
	hdr("From", e.from.String())
	hdr("To", strings.Join(addrs, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", e.s.SubjectPrefix+target.Title(msg.Markdown)))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", fmt.Sprintf("<%s@%s>", randomID(), e.from.Address[strings.LastIndex(e.from.Address, "@")+1:]))
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "8bit")
	hdr("Auto-Submitted", "auto-generated") // no out-of-office replies
	if msg.Route != "" {
		hdr("X-Stellwerk-Route", msg.Route)
	}
	if msg.Source != "" {
		hdr("X-Stellwerk-Source", msg.Source)
	}
	if msg.Priority == domain.PriorityHigh || msg.Priority == domain.PriorityUrgent {
		hdr("X-Priority", "1")
		hdr("Importance", "high")
	}
	b.WriteString("\r\n")
	for _, line := range strings.Split(strings.ReplaceAll(msg.Markdown, "\r\n", "\n"), "\n") {
		b.WriteString(line + "\r\n") // net/smtp does the dot-stuffing
	}
	if e.node != "" {
		b.WriteString("\r\n-- \r\nsent by stellwerk on " + e.node + "\r\n")
	}
	return b.Bytes()
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
