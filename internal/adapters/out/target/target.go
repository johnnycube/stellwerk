// Package target holds the outbound adapters that implement ports.Sender.
//
// A target type registers a Factory. Config creates named target instances,
// each with one or more accounts. Every account becomes its own sender
// (own login, own queue), addressed as "target/account".
//
// To add a type (ntfy, email, Telegram, ...), create a package that calls
// Register in init and import it in internal/adapters/out/target/all.
package target

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
	"go.yaml.in/yaml/v4"
)

// Target is a sender for one account.
type Target interface {
	ports.Sender
}

// Optional interfaces a target may implement.
type (
	Runner  interface{ Run(ctx context.Context) } // background loop, e.g. Matrix sync
	Readier interface{ Ready() bool }             // can deliver yet, used by /readyz
	Closer  interface{ Close() error }
)

type Env struct {
	Target  string // instance name from config
	Account string // account name within the instance
	NodeID  string // per-node state (Matrix device) uses it
	Log     *slog.Logger
	// Commands handles chat commands for targets that can receive them
	// (Matrix with commands: true). It returns the reply and whether to
	// send it. Nil: no commands.
	Commands CommandFunc
}

// CommandFunc runs a chat command: the message ID, who sent it and the text.
type CommandFunc func(ctx context.Context, id, sender, text string) (reply string, answer bool)

// HealthChecker is an optional interface: a target that can tell why
// delivery to some addresses will not work (not in the room, room not
// encrypted, device not verified).
type HealthChecker interface {
	Check(ctx context.Context, addresses []string) []string
}

// Expand replaces {target}, {account} and {node} in a string.
func (e Env) Expand(s string) string {
	return strings.NewReplacer("{target}", e.Target, "{account}", e.Account, "{node}", e.NodeID).Replace(s)
}

// Factory builds a sender from the account's settings: the instance's shared
// settings with the account's own settings merged over them.
type Factory func(ctx context.Context, env Env, settings *yaml.Node) (Target, error)

type Info struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Address     string `json:"address"` // what a destination address means
}

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
	infos     = map[string]Info{}
)

func Register(info Info, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[info.Type]; dup {
		panic("target type registered twice: " + info.Type)
	}
	factories[info.Type] = f
	infos[info.Type] = info
}

func New(ctx context.Context, typ string, env Env, settings *yaml.Node) (Target, error) {
	mu.RLock()
	f, ok := factories[typ]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown target type %q (known: %v)", typ, Types())
	}
	return f(ctx, env, settings)
}

func Types() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func Infos() map[string]Info {
	mu.RLock()
	defer mu.RUnlock()
	out := map[string]Info{}
	for k, v := range infos {
		out[k] = v
	}
	return out
}

// DecodeSettings is a helper for factories.
func DecodeSettings(n *yaml.Node, into any) error {
	if n == nil || n.Kind == 0 {
		return nil
	}
	return n.Decode(into)
}

// MergeSettings returns shared with the keys of account set over it.
// Both must be mappings (or empty). Values are replaced, not deep-merged.
func MergeSettings(shared, account *yaml.Node) (*yaml.Node, error) {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	idx := map[string]int{}
	for _, n := range []*yaml.Node{shared, account} {
		if n == nil || n.Kind == 0 {
			continue
		}
		if n.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("settings must be a mapping")
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if j, ok := idx[k.Value]; ok {
				out.Content[j+1] = v
				continue
			}
			idx[k.Value] = len(out.Content)
			out.Content = append(out.Content, k, v)
		}
	}
	return out, nil
}

// Title is the first non-empty line of a Markdown message without Markdown
// marks, at most 120 runes. For mail subjects and push titles.
func Title(md string) string {
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "#>-* ")
		line = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "", "_", "").Replace(line)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 120 {
			line = string(r[:119]) + "…"
		}
		return line
	}
	return "message"
}

// StatusError turns a non-2xx HTTP response into an error the core can act
// on: 429 carries Retry-After, 408, 425 and 5xx are worth retrying, every
// other 4xx is permanent (wrong URL, token or payload will not fix itself).
// It reads up to 300 bytes of the body for the message.
func StatusError(prefix string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	err := fmt.Errorf("%s answered %s: %s", prefix, resp.Status, bytes.TrimSpace(b))
	switch c := resp.StatusCode; {
	case c == http.StatusTooManyRequests:
		return ports.RetryAfter(err, ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now()))
	case c == http.StatusRequestTimeout || c == http.StatusTooEarly || c >= 500:
		return err
	case c >= 400:
		return ports.Permanent(err)
	}
	return err
}

// ParseRetryAfter reads seconds or an HTTP date. 0 if absent or invalid.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
