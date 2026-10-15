package main

import (
	"errors"
	"path/filepath"
	"testing"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/store"
)

func TestCheckDatabaseReportsOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "check.db")
	cfg := config.Config{}
	cfg.Database.Path = path
	if err := checkDatabase(cfg, 0); err != nil {
		t.Fatal(err)
	}
	owner, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := checkDatabase(cfg, 0); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("expected ErrLocked while another owner holds the database, got %v", err)
	}
}
