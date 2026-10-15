package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestJournalMigrationAndOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	ctx := context.Background()
	for _, journal := range []string{"wal", "delete", "delete", "wal"} {
		s, err := OpenWithJournal(path, journal, false)
		if err != nil {
			t.Fatal(err)
		}
		if other, err := OpenWithJournal(path, journal, false); err == nil {
			other.Close()
			t.Fatal("second owner accepted")
		}
		var mode, integrity string
		var sync int
		if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != journal {
			t.Fatalf("mode=%s err=%v", mode, err)
		}
		// Force replacement of the connection: durability pragmas must persist.
		s.db.SetMaxIdleConns(0)
		wantSync := 2
		if journal == "delete" {
			wantSync = 3
		}
		if err := s.db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil || sync != wantSync {
			t.Fatalf("synchronous=%d err=%v", sync, err)
		}
		if err := s.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
			t.Fatalf("integrity=%s err=%v", integrity, err)
		}
		if journal == "delete" {
			var got string
			if err := s.Get(ctx, "test", "persist", &got); err != nil || got != "value" {
				t.Fatalf("migration lost data: %q %v", got, err)
			}
		}
		if err := s.Put(ctx, "test", "persist", "value"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidJournal(t *testing.T) {
	if s, err := OpenWithJournal(filepath.Join(t.TempDir(), "db"), "off", false); err == nil {
		s.Close()
		t.Fatal("unsafe journaling accepted")
	}
}
