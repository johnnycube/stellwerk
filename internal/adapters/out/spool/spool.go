// Package spool is the durable outbox (ports.Outbox) in a SQLite file.
// Deliveries are written with synchronous=FULL, so an accepted delivery
// survives a crash or power loss, not only a clean restart.
//
// The stored format is this package's own record type, versioned, and not
// the domain struct, so refactoring the domain does not break old spools.
package spool

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
)

const version = 1

type SQLite struct {
	db *sql.DB
}

var _ ports.Outbox = (*SQLite)(nil)

// Open opens or creates the spool file.
func Open(path string) (*SQLite, error) {
	if path == "" {
		return nil, errors.New("spool: path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	db.SetMaxOpenConns(1) // one writer, no lock fights
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS deliveries (
		seq     INTEGER PRIMARY KEY AUTOINCREMENT,
		id      TEXT NOT NULL UNIQUE,
		created INTEGER NOT NULL,
		version INTEGER NOT NULL,
		body    BLOB NOT NULL)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("spool %s: %w", path, err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS silences (id TEXT PRIMARY KEY, body BLOB NOT NULL)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("spool %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600) // may contain alert texts
	return &SQLite{db: db}, nil
}

func (s *SQLite) Put(d domain.Delivery) error {
	if d.ID == "" {
		return errors.New("spool: delivery has no ID")
	}
	body, err := json.Marshal(toRecord(d))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO deliveries (id, created, version, body) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET body = excluded.body, version = excluded.version`,
		d.ID, time.Now().UnixMilli(), version, body)
	return err
}

func (s *SQLite) Done(id string) error {
	_, err := s.db.Exec(`DELETE FROM deliveries WHERE id = ?`, id)
	return err
}

func (s *SQLite) Pending() ([]domain.Delivery, error) {
	rows, err := s.db.Query(`SELECT id, version, body FROM deliveries ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Delivery
	for rows.Next() {
		var (
			id   string
			v    int
			body []byte
		)
		if err := rows.Scan(&id, &v, &body); err != nil {
			return nil, err
		}
		if v != version {
			return nil, fmt.Errorf("spool: delivery %s has format version %d, this build reads %d", id, v, version)
		}
		var r record
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("spool: delivery %s: %w", id, err)
		}
		out = append(out, r.domain())
	}
	return out, rows.Err()
}

// Len counts what is not done yet, for metrics.
func (s *SQLite) Len() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&n)
	return n
}

func (s *SQLite) Close() error { return s.db.Close() }

// record is the stored form, version 1.
type record struct {
	ID          string   `json:"id"`
	Route       string   `json:"route"`
	Destination string   `json:"destination"`
	Endpoint    string   `json:"endpoint"`
	Address     string   `json:"address,omitempty"`
	Markdown    string   `json:"markdown"`
	Loud        bool     `json:"loud,omitempty"`
	Priority    string   `json:"priority,omitempty"`
	Mentions    []string `json:"mentions,omitempty"`
	Source      string   `json:"source,omitempty"`
	MsgRoute    string   `json:"msgRoute,omitempty"`
	EntryID     string   `json:"entryId,omitempty"`
	Index       int      `json:"index,omitempty"`
	FallbackFor string   `json:"fallbackFor,omitempty"`
	Fallback    []record `json:"fallback,omitempty"`
	Created     int64    `json:"created,omitempty"` // unix ms
}

func toRecord(d domain.Delivery) record {
	r := record{ID: d.ID, Route: d.Route, Destination: d.Destination, Endpoint: d.Endpoint, Address: d.Address,
		Markdown: d.Message.Markdown, Loud: d.Message.Loud, Priority: d.Message.Priority, Mentions: d.Message.Mentions,
		Source: d.Message.Source, MsgRoute: d.Message.Route, EntryID: d.EntryID, Index: d.Index, FallbackFor: d.FallbackFor}
	if !d.Created.IsZero() {
		r.Created = d.Created.UnixMilli()
	}
	for _, fb := range d.Fallback {
		r.Fallback = append(r.Fallback, toRecord(fb))
	}
	return r
}

func (r record) domain() domain.Delivery {
	d := domain.Delivery{ID: r.ID, Route: r.Route, Destination: r.Destination, Endpoint: r.Endpoint, Address: r.Address,
		Message: domain.Message{Markdown: r.Markdown, Loud: r.Loud, Priority: r.Priority, Mentions: r.Mentions, Source: r.Source, Route: r.MsgRoute},
		EntryID: r.EntryID, Index: r.Index, FallbackFor: r.FallbackFor}
	if r.Created != 0 {
		d.Created = time.UnixMilli(r.Created).UTC()
	}
	for _, fb := range r.Fallback {
		d.Fallback = append(d.Fallback, fb.domain())
	}
	return d
}

// Silences returns a silence store in the same file, so silences survive
// restarts together with the deliveries.
func (s *SQLite) Silences() *Silences { return &Silences{db: s.db} }

type Silences struct{ db *sql.DB }

var _ ports.SilenceStore = (*Silences)(nil)

func (s *Silences) Save(x domain.Silence) error {
	body, err := json.Marshal(x)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO silences (id, body) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET body = excluded.body`, x.ID, body)
	return err
}

func (s *Silences) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM silences WHERE id = ?`, id)
	return err
}

func (s *Silences) All() ([]domain.Silence, error) {
	rows, err := s.db.Query(`SELECT body FROM silences ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Silence
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var x domain.Silence
		if err := json.Unmarshal(body, &x); err != nil {
			return nil, fmt.Errorf("spool: silence: %w", err)
		}
		x.Starts, x.Ends = x.Starts.UTC(), x.Ends.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}
