package email

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
	"go.yaml.in/yaml/v3"
)

// smtpServer is a small SMTP server for tests: EHLO, STARTTLS, AUTH PLAIN,
// MAIL, RCPT, DATA, QUIT.
type smtpServer struct {
	ln       net.Listener
	cert     tls.Certificate
	implicit bool
	starttls bool
	user     string // empty: no AUTH needed
	pass     string
	refuse   atomic.Bool // RCPT gets 550
	mu       sync.Mutex
	mails    []received
}

type received struct {
	from, auth string
	to         []string
	data       string
}

func newSMTP(t *testing.T, implicit, starttls bool, user, pass string) *smtpServer {
	t.Helper()
	s := &smtpServer{cert: selfSigned(t), implicit: implicit, starttls: starttls, user: user, pass: pass}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if implicit {
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{s.cert}})
	}
	s.ln = ln
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *smtpServer) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *smtpServer) received() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]received(nil), s.mails...)
}

func (s *smtpServer) serve(c net.Conn) {
	defer c.Close()
	r, w := bufio.NewReader(c), c
	say := func(l string) { io.WriteString(w, l+"\r\n") }
	say("220 test ESMTP")
	var cur received
	authed := s.user == ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			io.WriteString(w, "250-test\r\n")
			if s.starttls {
				io.WriteString(w, "250-STARTTLS\r\n")
			}
			say("250 AUTH PLAIN")
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{s.cert}})
			if tc.Handshake() != nil {
				return
			}
			c, r, w = tc, bufio.NewReader(tc), tc
		case "AUTH":
			parts := strings.Fields(line)
			raw, _ := base64.StdEncoding.DecodeString(parts[len(parts)-1])
			f := strings.Split(string(raw), "\x00")
			if len(f) == 3 && f[1] == s.user && f[2] == s.pass {
				authed, cur.auth = true, f[1]
				say("235 ok")
			} else {
				say("535 bad credentials")
			}
		case "MAIL":
			if !authed {
				say("530 auth required")
				continue
			}
			cur.from = between(line)
			say("250 ok")
		case "RCPT":
			if s.refuse.Load() {
				say("550 no such user")
				continue
			}
			cur.to = append(cur.to, between(line))
			say("250 ok")
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".")) // undo dot-stuffing
			}
			cur.data = b.String()
			s.mu.Lock()
			s.mails = append(s.mails, cur)
			s.mu.Unlock()
			cur = received{auth: cur.auth}
			say("250 queued")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func between(l string) string {
	i, j := strings.Index(l, "<"), strings.Index(l, ">")
	if i < 0 || j < i {
		return ""
	}
	return l[i+1 : j]
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newEmail(t *testing.T, yml string) target.Target {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(yml), &n); err != nil {
		t.Fatal(err)
	}
	e, err := target.New(context.Background(), "email", target.Env{Target: "mail", Account: "default", NodeID: "n1", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, n.Content[0])
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func settings(s *smtpServer, extra string) string {
	return "host: 127.0.0.1\nport: " + itoa(s.port()) + "\nfrom: \"stellwerk <alerts@example.org>\"\ninsecure_skip_verify: true\n" + extra
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestEmailSenderContract(t *testing.T) {
	portstest.SenderContract(t, func(t *testing.T) portstest.SenderUnderTest {
		s := newSMTP(t, false, false, "", "")
		e := newEmail(t, settings(s, "tls: none\nsubject_prefix: \"x \""))
		return portstest.SenderUnderTest{
			Sender: e,
			Received: func() []portstest.Sent {
				var out []portstest.Sent
				for _, m := range s.received() {
					_, body, _ := strings.Cut(m.data, "\r\n\r\n")
					body, _, _ = strings.Cut(body, "\r\n-- \r\n")
					out = append(out, portstest.Sent{Address: strings.Join(m.to, ","), Msg: domain.Message{Markdown: strings.ReplaceAll(strings.TrimSuffix(body, "\r\n"), "\r\n", "\n")}})
				}
				return out
			},
			Break:       func() { s.refuse.Store(true) },
			AddressHint: "me@example.org",
			BodyOnly:    true,
		}
	})
}

func TestEmailStartTLSWithAuth(t *testing.T) {
	s := newSMTP(t, false, true, "bot", "secret")
	e := newEmail(t, settings(s, "username: bot\npassword: secret"))
	msg := domain.Message{Markdown: "🔥 **Disk full** FIRING\n- on node a\n.\n", Priority: domain.PriorityUrgent, Route: "crit", Source: "am"}
	if err := e.Send(context.Background(), "me@example.org, Ops <ops@example.org>", msg); err != nil {
		t.Fatal(err)
	}
	got := s.received()
	if len(got) != 1 || got[0].auth != "bot" || got[0].from != "alerts@example.org" || len(got[0].to) != 2 || got[0].to[1] != "ops@example.org" {
		t.Fatalf("%+v", got)
	}
	d := got[0].data
	for _, want := range []string{
		"Subject: =?utf-8?q?", "X-Priority: 1", "X-Stellwerk-Route: crit", "Auto-Submitted: auto-generated",
		"Content-Type: text/plain; charset=utf-8", "**Disk full**", "\r\n.\r\n", "sent by stellwerk on n1",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in\n%s", want, d)
		}
	}
}

func TestEmailImplicitTLS(t *testing.T) {
	s := newSMTP(t, true, false, "", "")
	e := newEmail(t, settings(s, "tls: implicit"))
	if err := e.Send(context.Background(), "me@example.org", domain.Message{Markdown: "hi", Priority: domain.PriorityNormal}); err != nil {
		t.Fatal(err)
	}
	if got := s.received(); len(got) != 1 || strings.Contains(got[0].data, "X-Priority") {
		t.Errorf("%+v", got)
	}
}

func TestEmailErrors(t *testing.T) {
	plain := newSMTP(t, false, false, "", "")
	if err := newEmail(t, settings(plain, "")).Send(context.Background(), "me@x.org", domain.Message{Markdown: "x"}); err == nil ||
		!strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("starttls required by default: %v", err)
	}
	auth := newSMTP(t, false, true, "bot", "secret")
	if err := newEmail(t, settings(auth, "username: bot\npassword: wrong")).Send(context.Background(), "me@x.org", domain.Message{Markdown: "x"}); err == nil ||
		!strings.Contains(err.Error(), "auth") {
		t.Errorf("bad password: %v", err)
	}
	e := newEmail(t, settings(plain, "tls: none"))
	for _, addr := range []string{"", "not an address"} {
		if err := e.Send(context.Background(), addr, domain.Message{}); err == nil {
			t.Errorf("address %q must fail", addr)
		}
	}
	closed := newEmail(t, "host: 127.0.0.1\nport: 1\nfrom: a@b.c\ntls: none\ntimeout: 1s")
	if err := closed.Send(context.Background(), "me@x.org", domain.Message{}); err == nil {
		t.Error("connection refused must fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Send(ctx, "me@x.org", domain.Message{}); err == nil {
		t.Error("cancelled context must fail")
	}
}

func TestEmailSettings(t *testing.T) {
	env := target.Env{Target: "mail", Account: "default"}
	for name, s := range map[string]Settings{
		"no host":  {From: "a@b.c"},
		"no from":  {Host: "h"},
		"bad from": {Host: "h", From: "nope"},
		"bad tls":  {Host: "h", From: "a@b.c", TLS: "maybe"},
	} {
		if _, err := New(env, s); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	e, _ := New(env, Settings{Host: "h", From: "a@b.c", TLS: "implicit"})
	if e.s.Port != 465 || e.s.Timeout != 20*time.Second || e.s.SubjectPrefix != "[stellwerk] " {
		t.Errorf("%+v", e.s)
	}
	e, _ = New(env, Settings{Host: "h", From: "a@b.c"})
	if e.s.Port != 587 || e.s.TLS != "starttls" {
		t.Errorf("%+v", e.s)
	}
}

func TestEmailRejectedRecipientIsPermanent(t *testing.T) {
	s := newSMTP(t, false, false, "", "")
	s.refuse.Store(true)
	err := newEmail(t, settings(s, "tls: none")).Send(context.Background(), "me@x.org", domain.Message{Markdown: "x"})
	if !ports.IsPermanent(err) {
		t.Errorf("550 must be permanent: %v", err)
	}
	if err := newEmail(t, settings(s, "tls: none")).Send(context.Background(), "", domain.Message{}); !ports.IsPermanent(err) {
		t.Error("no recipients is permanent")
	}
	closed := newEmail(t, "host: 127.0.0.1\nport: 1\nfrom: a@b.c\ntls: none\ntimeout: 1s")
	if err := closed.Send(context.Background(), "me@x.org", domain.Message{}); ports.IsPermanent(err) {
		t.Error("connection refused is worth retrying")
	}
}
