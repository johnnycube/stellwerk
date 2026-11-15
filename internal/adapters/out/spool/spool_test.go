package spool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func TestSQLiteOutboxContract(t *testing.T) {
	paths := map[*testing.T]string{}
	portstest.OutboxContract(t, func(t *testing.T) ports.Outbox {
		p, ok := paths[t]
		if !ok {
			p = filepath.Join(t.TempDir(), "sub", "spool.db")
			paths[t] = p
		}
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestSQLiteDetails(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spool.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if err := s.Put(domain.Delivery{}); err == nil {
		t.Error("a delivery without ID must be refused")
	}
	_ = s.Put(domain.Delivery{ID: "a"})
	if s.Len() != 1 {
		t.Error("len")
	}
	if _, err := Open(""); err == nil {
		t.Error("empty path")
	}
	if _, err := s.db.Exec(`UPDATE deliveries SET version = 99`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pending(); err == nil {
		t.Error("an unknown format version must not be read as if it were known")
	}
	if _, err := s.db.Exec(`UPDATE deliveries SET version = 1, body = 'not json'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pending(); err == nil {
		t.Error("broken body")
	}
}

func TestOpenFailsOnUnusablePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := Open(filepath.Join(file, "spool.db")); err == nil {
		t.Error("a file as directory must fail")
	}
}

func TestSQLiteSilenceStoreContract(t *testing.T) {
	paths := map[*testing.T]string{}
	portstest.SilenceStoreContract(t, func(t *testing.T) ports.SilenceStore {
		p, ok := paths[t]
		if !ok {
			p = filepath.Join(t.TempDir(), "spool.db")
			paths[t] = p
		}
		s, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s.Silences()
	})
}
