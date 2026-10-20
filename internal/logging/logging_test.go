package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"kubeflow-admin/internal/config"
)

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testConfig(path string, sizeMiB, backups, ageDays int) config.Logging {
	return config.Logging{Path: path, MaxSizeMiB: sizeMiB, MaxBackups: backups, MaxAgeDays: ageDays}
}

func listBackups(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if _, ok := backupIndex(filepath.Base(path), entry.Name()); ok {
			out = append(out, filepath.Join(filepath.Dir(path), entry.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file", path)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("%s mode = %o, want 0600", path, fi.Mode().Perm())
	}
}

func TestSetupStdoutDefault(t *testing.T) {
	oldStdout := os.Stdout
	oldLogger := slog.Default()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
		slog.SetDefault(oldLogger)
		w.Close()
	})

	closer, err := Setup(config.Logging{})
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("stdout default", "answer", 42)
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(out, &record); err != nil {
		t.Fatalf("stdout was not one JSON record: %v: %q", err, out)
	}
	if record["msg"] != "stdout default" || record["answer"] != float64(42) {
		t.Fatalf("unexpected record: %v", record)
	}
}

func TestSetupCreatesPrivateFileAndDirectory(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "nested", "admin.log")
	oldLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	closer, err := Setup(testConfig(path, 1, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("private file", "value", "ok")
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	assertPrivateFile(t, path)
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0700 {
		t.Fatalf("parent mode = %o, want 0700", fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("file was not JSON: %v: %q", err, data)
	}
	if record["msg"] != "private file" {
		t.Fatalf("unexpected record: %v", record)
	}
}

func TestSetupRotatesAtConfiguredSize(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "admin.log")
	oldLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	closer, err := Setup(testConfig(path, 1, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 48<<10)
	for i := 0; i < 30; i++ {
		slog.Info("bulk", "payload", payload)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	assertPrivateFile(t, path)
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Size() > 1<<20 {
		t.Fatalf("active file size %d exceeds configured limit", fi.Size())
	}
	backups := listBackups(t, path)
	if len(backups) == 0 {
		t.Fatal("expected at least one rotated backup")
	}
	if len(backups) > 2 {
		t.Fatalf("backup count = %d, want <= 2: %v", len(backups), backups)
	}
	for _, backup := range backups {
		assertPrivateFile(t, backup)
	}
}

func TestSizeCountAndConcurrentRotation(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "admin.log")
	w, err := newRotatingWriter(path, 2048, 3, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	const writers = 8
	const writesPerWriter = 80
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < writesPerWriter; i++ {
				line := fmt.Sprintf("%02d-%03d %s\n", id, i, strings.Repeat("w", 200))
				if _, err := w.Write([]byte(line)); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 2048 {
		t.Fatalf("active file size %d exceeds limit", fi.Size())
	}
	assertPrivateFile(t, path)
	backups := listBackups(t, path)
	if len(backups) != 3 {
		t.Fatalf("backup count = %d, want exactly 3: %v", len(backups), backups)
	}
	for _, backup := range backups {
		assertPrivateFile(t, backup)
		if fi, err := os.Stat(backup); err != nil {
			t.Fatal(err)
		} else if fi.Size() > 2048 {
			t.Fatalf("backup %s size %d exceeds limit", backup, fi.Size())
		}
	}
}

func TestAgeCleanupAtSetupAndRotation(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "admin.log")
	now := time.Now()

	oldAtSetup := path + ".1"
	if err := os.WriteFile(oldAtSetup, []byte("old setup backup"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := now.Add(-48 * time.Hour)
	if err := os.Chtimes(oldAtSetup, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	recent := path + ".2"
	if err := os.WriteFile(recent, []byte("recent backup"), 0600); err != nil {
		t.Fatal(err)
	}
	recentTime := now.Add(-time.Hour)
	if err := os.Chtimes(recent, recentTime, recentTime); err != nil {
		t.Fatal(err)
	}

	w, err := newRotatingWriter(path, 4096, 5, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldAtSetup); !os.IsNotExist(err) {
		t.Fatalf("old backup survived setup cleanup: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent backup was removed at setup: %v", err)
	}

	oldAtRotation := path + ".4"
	if err := os.WriteFile(oldAtRotation, []byte("old rotation backup"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldAtRotation, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := w.Write([]byte(strings.Repeat("r", 3000))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(oldAtRotation); !os.IsNotExist(err) {
		t.Fatalf("old backup survived rotation cleanup: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSymlinkActiveFileRejected(t *testing.T) {
	dir := privateTempDir(t)
	targetDir := privateTempDir(t)
	target := filepath.Join(targetDir, "target.log")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admin.log")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Setup(testConfig(path, 1, 2, 1)); err == nil {
		t.Fatal("symlink log path accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("symlink target modified: %q", data)
	}
}

func TestSymlinkBackupFailsClosed(t *testing.T) {
	dir := privateTempDir(t)
	targetDir := privateTempDir(t)
	target := filepath.Join(targetDir, "target.log")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admin.log")
	w, err := newRotatingWriter(path, 1024, 2, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".1"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := w.Write([]byte(strings.Repeat("a", 700))); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("b", 700))); err == nil {
		t.Fatal("rotation through a symlinked backup did not fail")
	}
	if err := w.Close(); err == nil {
		t.Fatal("close did not surface the rotation failure")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("symlink target modified: %q", data)
	}
}

func TestRejectsNonPrivateExistingState(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		dir := privateTempDir(t)
		path := filepath.Join(dir, "admin.log")
		if err := os.WriteFile(path, []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Setup(testConfig(path, 1, 2, 1)); err == nil {
			t.Fatal("group/world-readable log file accepted")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "keep" {
			t.Fatalf("rejected file was modified: %q", data)
		}
	})
	t.Run("backup", func(t *testing.T) {
		dir := privateTempDir(t)
		path := filepath.Join(dir, "admin.log")
		if err := os.WriteFile(path, []byte("active"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".1", []byte("backup"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := newRotatingWriter(path, 1024, 2, 24*time.Hour); err == nil {
			t.Fatal("group/world-readable backup accepted")
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := privateTempDir(t)
		openDir := filepath.Join(dir, "open")
		if err := os.Mkdir(openDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(openDir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := Setup(testConfig(filepath.Join(openDir, "admin.log"), 1, 2, 1)); err == nil {
			t.Fatal("group/world-accessible parent directory accepted")
		}
	})
}

func TestOversizedRecordRejectedWithoutLeakingContent(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "admin.log")
	w, err := newRotatingWriter(path, 1024, 2, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const secret = "SUPER_SECRET_VALUE"
	payload := secret + strings.Repeat("z", 32<<10)
	if n, err := w.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("oversized write: n=%d err=%v", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) > 1024 {
		t.Fatalf("oversized record produced %d bytes", len(data))
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "zzzz") {
		t.Fatal("rejected record content was persisted")
	}
	if !strings.Contains(string(data), "log record rejected") {
		t.Fatalf("missing rejection notice: %q", data)
	}
}

func TestCloseAndFailureSurfacing(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "admin.log")
	w, err := newRotatingWriter(path, 1024, 2, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("before close\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := w.Write([]byte("after close\n")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write after close = %v, want os.ErrClosed", err)
	}

	if _, err := Setup(testConfig(path, -1, 2, 1)); err == nil {
		t.Fatal("negative size accepted")
	}
	if closer, err := Setup(testConfig(path, 1, 0, 0)); err != nil {
		// Zero limits use WithDefaults; this checks the documented defaults.
		t.Fatalf("zero limits should use defaults: %v", err)
	} else if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(testConfig("relative.log", 1, 2, 1)); err == nil {
		t.Fatal("relative path accepted")
	}
	asDir := filepath.Join(dir, "isdir")
	if err := os.Mkdir(asDir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Setup(testConfig(asDir, 1, 2, 1)); err == nil {
		t.Fatal("path that is a directory accepted")
	}
}

func TestRotationErrorDoesNotContainRecord(t *testing.T) {
	dir := privateTempDir(t)
	targetDir := privateTempDir(t)
	target := filepath.Join(targetDir, "target.log")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admin.log")
	w, err := newRotatingWriter(path, 1024, 2, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path+".1"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	const secret = "TOKEN_MUST_NOT_APPEAR"
	if _, err := w.Write([]byte(strings.Repeat("a", 700))); err != nil {
		t.Fatal(err)
	}
	record := secret + strings.Repeat("b", 700-len(secret))
	if _, err := w.Write([]byte(record)); err == nil {
		t.Fatal("rotation failure not surfaced")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("write error leaked record content: %v", err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("close did not surface rotation failure")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("close error leaked record content: %v", err)
	}
}

func TestPermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can create files in a read-only directory")
	}
	dir := privateTempDir(t)
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0700) })
	if _, err := Setup(testConfig(filepath.Join(locked, "admin.log"), 1, 2, 1)); err == nil {
		t.Fatal("unwritable directory accepted")
	}
}

func TestPrivateModesWithArbitraryUmask(t *testing.T) {
	for _, mask := range []int{0, 0777} {
		t.Run(fmt.Sprintf("umask-%04o", mask), func(t *testing.T) {
			dir := t.TempDir()
			old := syscall.Umask(mask)
			defer syscall.Umask(old)

			path := filepath.Join(dir, "nested", "deeper", "admin.log")
			oldLogger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(oldLogger) })
			closer, err := Setup(testConfig(path, 1, 2, 1))
			if err != nil {
				t.Fatal(err)
			}
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
			assertPrivateFile(t, path)
			for _, parent := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
				fi, err := os.Stat(parent)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != 0700 {
					t.Fatalf("parent %s mode with umask %04o = %o, want 0700", parent, mask, fi.Mode().Perm())
				}
			}
		})
	}
}

func TestSetupRotatesFullPreexistingActive(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "admin.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("p", 1024)), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := newRotatingWriter(path, 1024, 2, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Fatalf("active size after setup rotation = %d, want 0", fi.Size())
	}
	backup, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if backup.Size() != 1024 {
		t.Fatalf("rotated backup size = %d, want 1024", backup.Size())
	}
	assertPrivateFile(t, path+".1")
}

func TestBackupIndexRejectsUnrelatedNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		ok   bool
	}{
		{"admin.log.1", 1, true},
		{"admin.log.12", 12, true},
		{"admin.log.0", 0, false},
		{"admin.log.01", 0, false},
		{"admin.log.old", 0, false},
		{"admin.log", 0, false},
		{"other.log.1", 0, false},
	} {
		got, ok := backupIndex("admin.log", tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("backupIndex(%q) = %d,%v want %d,%v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
