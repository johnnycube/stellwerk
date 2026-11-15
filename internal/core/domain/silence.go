package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Silence mutes matching events for a while, e.g. during maintenance.
// Matching events are recorded but not delivered. A heartbeat alarm is
// muted by a silence that matches the heartbeat's source and the label
// heartbeat=<name>.
type Silence struct {
	ID        string    `json:"id"`
	Match     Match     `json:"match"`
	Starts    time.Time `json:"starts"`
	Ends      time.Time `json:"ends"`
	CreatedBy string    `json:"createdBy,omitempty"`
	Comment   string    `json:"comment,omitempty"`
}

// Active reports whether the silence applies at now.
func (s Silence) Active(now time.Time) bool { return !now.Before(s.Starts) && now.Before(s.Ends) }

// Describe is a one-line summary for chat and logs.
func (s Silence) Describe() string { return FormatMatchers(s.Match) }

// ParseMatchers reads matchers written as words: "source=k8s*",
// "status=firing" and "<label>=<regex>" for anything else. Example:
//
//	source=alertmanager severity=warning|info namespace=!prod
//
// A silence needs at least one matcher, so it can never mute everything
// by accident.
func ParseMatchers(s string) (Match, error) {
	m := Match{Labels: map[string]string{}}
	words := strings.Fields(s)
	if len(words) == 0 {
		return m, fmt.Errorf("give at least one matcher, e.g. source=k8s or severity=warning")
	}
	for _, w := range words {
		k, v, ok := strings.Cut(w, "=")
		if !ok || k == "" || v == "" {
			return m, fmt.Errorf("%q: write key=value, e.g. source=k8s", w)
		}
		switch k {
		case "source":
			m.Source = v
		case "status":
			m.Status = v
		default:
			m.Labels[k] = v
		}
	}
	if _, err := CompileMatch(m); err != nil {
		return m, err
	}
	return m, nil
}

// FormatMatchers is the inverse of ParseMatchers.
func FormatMatchers(m Match) string {
	var parts []string
	if m.Source != "" {
		parts = append(parts, "source="+m.Source)
	}
	if m.Status != "" {
		parts = append(parts, "status="+m.Status)
	}
	for _, k := range sortedStrings(m.Labels) {
		parts = append(parts, k+"="+m.Labels[k])
	}
	return strings.Join(parts, " ")
}

func sortedStrings(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ParseDuration accepts Go durations plus days: "90m", "2h", "1d", "1d12h".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var total time.Duration
	if d, rest, ok := strings.Cut(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(d, "%d", &n); err != nil || n < 0 || fmt.Sprint(n) != d {
			return 0, fmt.Errorf("duration %q: use e.g. 30m, 2h or 1d", s)
		}
		total = time.Duration(n) * 24 * time.Hour
		s = rest
	}
	if s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("duration %q: use e.g. 30m, 2h or 1d", s)
		}
		total += d
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return total, nil
}
