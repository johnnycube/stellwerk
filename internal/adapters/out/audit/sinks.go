package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/johnnycube/stellwerk/internal/core/ports"
)

// File appends settled entries as JSON lines. When the file grows past
// MaxBytes it is renamed to <path>.1 (replacing an older one) and a new file
// is started.
type File struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
	log      *slog.Logger
}

func NewFile(path string, maxBytes int64, log *slog.Logger) (*File, error) {
	fl := &File{path: path, maxBytes: maxBytes, log: log}
	return fl, fl.open()
}

func (fl *File) open() error {
	f, err := os.OpenFile(fl.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("audit file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	fl.f, fl.size = f, st.Size()
	return nil
}

func (fl *File) Write(e ports.AuditEntry) {
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.maxBytes > 0 && fl.size+int64(len(line)) > fl.maxBytes && fl.size > 0 {
		fl.rotate()
	}
	if fl.f == nil {
		return
	}
	n, err := fl.f.Write(line)
	fl.size += int64(n)
	if err != nil && fl.log != nil {
		fl.log.Error("audit file write failed", "err", err)
	}
}

func (fl *File) rotate() {
	fl.f.Close()
	fl.f = nil
	if err := os.Rename(fl.path, fl.path+".1"); err != nil && fl.log != nil {
		fl.log.Error("audit file rotation failed", "err", err)
	}
	if err := fl.open(); err != nil && fl.log != nil {
		fl.log.Error("audit file reopen failed", "err", err)
	}
}

func (fl *File) Close() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.f == nil {
		return nil
	}
	return fl.f.Close()
}

// Log writes one log line per settled entry, for log collectors.
type Log struct{ L *slog.Logger }

func (l Log) Write(e ports.AuditEntry) {
	attrs := []any{"audit_id", e.ID, "source", e.Source, "verdict", e.Verdict}
	if e.Status != "" {
		attrs = append(attrs, "status", e.Status)
	}
	if e.Reason != "" {
		attrs = append(attrs, "reason", e.Reason)
	}
	var routes, sent, failed []string
	for _, t := range e.Trace {
		if t.Matched {
			routes = append(routes, t.Route)
		}
	}
	for _, d := range e.Deliveries {
		if d.Status == "sent" {
			sent = append(sent, d.Destination)
			continue
		}
		msg := d.Destination
		if n := len(d.Attempts); n > 0 {
			msg += ": " + d.Attempts[n-1].Error
		}
		failed = append(failed, msg)
	}
	if len(routes) > 0 {
		attrs = append(attrs, "rules", strings.Join(routes, ","))
	}
	if len(sent) > 0 {
		attrs = append(attrs, "sent", strings.Join(sent, ","))
	}
	if len(failed) > 0 {
		attrs = append(attrs, "failed", strings.Join(failed, "; "))
	}
	level := slog.LevelInfo
	if e.Verdict == ports.AuditFailed || e.Verdict == ports.AuditError {
		level = slog.LevelWarn
	}
	l.L.Log(context.Background(), level, "audit", attrs...)
}
