// Package chatops is a driving adapter: commands typed in a chat room
// (Matrix today) that call the use cases. It knows nothing about Matrix,
// the chat target hands it text and sends back the reply.
//
//	!stellwerk mute 2h source=k8s severity=warning -- cluster upgrade
//	!stellwerk unmute s-1a2b
//	!stellwerk silences
//	!stellwerk status
//	!stellwerk canary
//	!stellwerk help
//
// "!sw" works as a short prefix. In a cluster every node gets the command:
// all of them apply it (silences get the same ID everywhere), only the
// leader answers.
package chatops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// Command is one chat message.
type Command struct {
	ID     string // the chat message's ID, makes the command idempotent
	Sender string
	Text   string
}

type Handler struct {
	Silences ports.ManageSilences
	Describe ports.DescribeTopology
	Canary   ports.RunCanary // nil: no canary
	Leader   func() bool     // nil: always answer
}

// IsCommand reports whether text is meant for stellwerk.
func IsCommand(text string) bool {
	_, ok := strip(text)
	return ok
}

func strip(text string) (string, bool) {
	text = strings.TrimSpace(text)
	for _, p := range []string{"!stellwerk", "!sw"} {
		if text == p || strings.HasPrefix(text, p+" ") {
			return strings.TrimSpace(strings.TrimPrefix(text, p)), true
		}
	}
	return "", false
}

// Handle runs a command and returns the Markdown reply, and whether this
// node should send it.
func (h *Handler) Handle(ctx context.Context, c Command) (string, bool) {
	rest, ok := strip(c.Text)
	if !ok {
		return "", false
	}
	answer := h.Leader == nil || h.Leader()
	verb, args, _ := strings.Cut(rest, " ")
	args = strings.TrimSpace(args)
	switch strings.ToLower(verb) {
	case "mute", "silence":
		return h.mute(c, args), answer
	case "unmute", "unsilence":
		if args == "" {
			return "Which one? `!stellwerk unmute <id>`, see `!stellwerk silences`.", answer
		}
		if err := h.Silences.Expire(args); err != nil {
			return "❌ " + err.Error(), answer
		}
		return fmt.Sprintf("🔔 Silence `%s` ended.", args), answer
	case "silences":
		return h.silences(), answer
	case "status":
		return h.status(), answer
	case "canary":
		if h.Canary == nil {
			return "No canary configured, set `canary.to`.", answer
		}
		if !answer {
			return "", false // only the leader runs it
		}
		res := h.Canary.Run(ctx)
		if res.OK {
			return fmt.Sprintf("🐤 Canary `%s` arrived.", res.Marker), true
		}
		return fmt.Sprintf("🚨 Canary `%s` failed: %s", res.Marker, strings.Join(res.Problems, "; ")), true
	default:
		return help, answer
	}
}

const help = "**stellwerk commands**\n" +
	"- `!stellwerk mute 2h source=k8s severity=warning -- upgrade`: mute matching events (source=, status= or any label, `heartbeat=<name>` for a heartbeat alarm)\n" +
	"- `!stellwerk unmute <id>`: end a silence\n" +
	"- `!stellwerk silences`: what is muted\n" +
	"- `!stellwerk status`: heartbeats, silences, canary\n" +
	"- `!stellwerk canary`: send the canary now\n" +
	"`!sw` works too."

func (h *Handler) mute(c Command, args string) string {
	spec, comment, _ := strings.Cut(args, "--")
	dur, matchers, _ := strings.Cut(strings.TrimSpace(spec), " ")
	if dur == "" || strings.TrimSpace(matchers) == "" {
		return "Write `!stellwerk mute <duration> <matchers> [-- comment]`, e.g. `!stellwerk mute 2h source=k8s -- upgrade`."
	}
	id := ""
	if c.ID != "" {
		sum := sha256.Sum256([]byte(c.ID))
		id = "s-" + hex.EncodeToString(sum[:4])
	}
	s, err := h.Silences.Silence(ports.SilenceRequest{ID: id, Matchers: matchers, For: dur, By: c.Sender, Comment: comment})
	if err != nil {
		return "❌ " + err.Error()
	}
	return fmt.Sprintf("🔕 Muted `%s` until %s (`%s`). End it with `!stellwerk unmute %s`.", s.Describe(), s.Ends.Format("Mon 15:04 MST"), s.ID, s.ID)
}

func (h *Handler) silences() string {
	act := h.Silences.Active()
	if len(act) == 0 {
		return "Nothing is muted."
	}
	var sb strings.Builder
	sb.WriteString("**Muted**")
	for _, s := range act {
		fmt.Fprintf(&sb, "\n- `%s` `%s` until %s", s.ID, s.Describe(), s.Ends.Format("Mon 15:04 MST"))
		if s.Comment != "" {
			fmt.Fprintf(&sb, ": %s", s.Comment)
		}
		if s.CreatedBy != "" {
			fmt.Fprintf(&sb, " (%s)", s.CreatedBy)
		}
	}
	return sb.String()
}

func (h *Handler) status() string {
	t := h.Describe.Describe()
	var sb strings.Builder
	sb.WriteString("**stellwerk status**")
	if t.Cluster.Enabled {
		fmt.Fprintf(&sb, "\nNode `%s`, leader `%s`.", t.Cluster.NodeID, t.Cluster.Leader)
	}
	hbs := t.Heartbeats
	sort.Slice(hbs, func(i, j int) bool { return hbs[i].Name < hbs[j].Name })
	for _, hb := range hbs {
		state := "✅"
		switch {
		case hb.Failed:
			state = "❌ failed"
		case hb.Missing:
			state = "🚨 missing"
		}
		seen := "never"
		if !hb.LastSeen.IsZero() {
			seen = time.Since(hb.LastSeen).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(&sb, "\n- heartbeat `%s` %s, last seen %s", hb.Name, state, seen)
	}
	fmt.Fprintf(&sb, "\n- %d active %s", len(t.Silences), map[bool]string{true: "silence", false: "silences"}[len(t.Silences) == 1])
	if h.Canary != nil {
		if last, ok := h.Canary.Last(); ok {
			fmt.Fprintf(&sb, "\n- last canary %s: %s", last.Time.Format("Mon 15:04"), map[bool]string{true: "arrived", false: "failed"}[last.OK])
		} else {
			sb.WriteString("\n- no canary sent yet")
		}
	}
	return sb.String()
}
