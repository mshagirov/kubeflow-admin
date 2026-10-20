package logging

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOversizedExistingActiveFileDoesNotEscapeBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admin.log")
	backup := path + ".99"
	if err := os.WriteFile(backup, []byte("preserve on failed startup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := newRotatingWriter(path, 1024, 2, 7*24*time.Hour)
	if err == nil {
		w.Close()
		t.Fatal("oversized existing log would exceed configured rotation budget")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 2048 {
		t.Fatal("existing diagnostic data must not be silently truncated")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("failed active-file validation must not prune backups")
	}
}

func TestOversizedRetainedBackupDoesNotEscapeBudget(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admin.log")
	if err := os.WriteFile(path+".1", make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := newRotatingWriter(path, 1024, 2, 7*24*time.Hour)
	if err == nil {
		w.Close()
		t.Fatal("retained backup exceeds configured rotation budget")
	}
}
