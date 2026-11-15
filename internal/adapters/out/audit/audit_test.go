package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func TestMemoryAuditStoreContract(t *testing.T) {
	portstest.AuditStoreContract(t, func() ports.AuditStore { return NewMemory(100) })
}

type collect struct {
	mu  sync.Mutex
	got []ports.AuditEntry
}

func (c *collect) Write(e ports.AuditEntry) { c.mu.Lock(); c.got = append(c.got, e); c.mu.Unlock() }

func TestMemoryKeepsTheNewest(t *testing.T) {
	m := NewMemory(3)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		m.Record(ports.AuditEntry{ID: id, Verdict: ports.AuditUnmatched})
	}
	if _, ok := m.Get("1"); ok || m.Len() != 3 || m.Evicted() != 2 {
		t.Errorf("len %d evicted %d", m.Len(), m.Evicted())
	}
	if l := m.List(ports.AuditQuery{}); l[0].ID != "5" || l[2].ID != "3" {
		t.Errorf("%+v", l)
	}
	m.Finish("1", 0, time.Now(), nil) // evicted: ignored
}

func TestSinksGetSettledEntriesOnce(t *testing.T) {
	c := &collect{}
	m := NewMemory(10, c)
	m.Record(ports.AuditEntry{ID: "dropped", Verdict: ports.AuditDropped})
	m.Record(ports.AuditEntry{ID: "p", Verdict: ports.AuditPending, Deliveries: []ports.AuditDelivery{{Status: "pending"}, {Status: "pending"}}})
	if len(c.got) != 1 {
		t.Fatalf("pending entries are not written yet: %d", len(c.got))
	}
	m.Finish("p", 0, time.Now(), nil)
	m.Finish("p", 1, time.Now(), errors.New("x"))
	m.Finish("p", 1, time.Now(), nil)
	if len(c.got) != 2 || c.got[1].ID != "p" || c.got[1].Verdict != ports.AuditFailed {
		t.Errorf("sink got %+v", c.got)
	}
}

func TestFileSinkWritesJSONLinesAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	f, err := NewFile(path, 600, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMemory(100, f)
	for i := 0; i < 10; i++ {
		m.Record(ports.AuditEntry{
			ID: strings.Repeat("x", 3) + string(rune('a'+i)), Time: time.Unix(0, 0).UTC(), Source: "am", Verdict: ports.AuditDropped,
			Trace: []domain.TraceStep{{Route: "noise", Matched: true, Action: "drop"}},
		})
	}
	f.Close()
	read := func(p string) []ports.AuditEntry {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var out []ports.AuditEntry
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			var e ports.AuditEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("bad line %q: %v", sc.Text(), err)
			}
			out = append(out, e)
		}
		return out
	}
	cur, old := read(path), read(path+".1")
	if len(cur)+len(old) < 4 || len(cur) == 0 || len(old) == 0 {
		t.Errorf("rotation: current %d, rotated %d", len(cur), len(old))
	}
	if last := cur[len(cur)-1]; last.ID != "xxxj" || last.Trace[0].Route != "noise" {
		t.Errorf("last line %+v", last)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("audit files may contain payloads, mode %v", st.Mode().Perm())
	}
}

func TestFileSinkBadPath(t *testing.T) {
	if _, err := NewFile("/nonexistent/dir/audit.jsonl", 0, nil); err == nil {
		t.Error("expected error")
	}
}

func TestLogSink(t *testing.T) {
	var buf bytes.Buffer
	l := Log{L: slog.New(slog.NewTextHandler(&buf, nil))}
	l.Write(ports.AuditEntry{
		ID: "n-1", Source: "am", Verdict: ports.AuditFailed, Status: "firing",
		Trace: []domain.TraceStep{{Route: "critical", Matched: true}, {Route: "rest"}},
		Deliveries: []ports.AuditDelivery{
			{Destination: "critical", Status: "sent"},
			{Destination: "pager", Status: "failed", Attempts: []ports.AuditAttempt{{Error: "503 Service Unavailable"}}},
		},
	})
	out := buf.String()
	for _, want := range []string{"level=WARN", "msg=audit", "audit_id=n-1", "verdict=failed", "rules=critical", "sent=critical", `failed="pager: 503 Service Unavailable"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}
