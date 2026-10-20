// Package logging installs the application's structured JSON logger.
//
// Stdout is the default sink: in Kubernetes the kubelet owns container-log
// rotation, so the application does not need a writable log volume. File output
// is opt-in through the frozen config.Logging contract. When enabled it uses a
// private regular file and numbered, size/age-bounded backups.
//
// This package only handles process diagnostics. It does not rotate SQLite,
// operation or audit records; audit retention remains a transactional database
// concern. Callers must keep tokens, secret values, full resource manifests and
// complete audit payloads out of log records in the first place. Rotation is
// local to one process/path: the application still runs one writer, and file
// logs are not an audit trail or a durable off-node backup.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"kubeflow-admin/internal/config"
)

// defaultMaxRecordBytes bounds a single write handed to the rotating sink.
// The writer rejects a larger record with a small fixed notice instead of
// copying or retaining the oversized payload in memory.
const defaultMaxRecordBytes = 1 << 20

// Setup configures the process-wide slog logger and returns a closer for the
// selected sink. An empty cfg.Path keeps JSON logs on stdout and returns a
// no-op closer. A non-empty path must name a private regular file (or a missing
// file in a private directory); setup fails closed rather than following a
// symlink, accepting group/world-accessible files, or silently falling back to
// stdout.
//
// Callers should treat a setup error as fatal. The previous logger is left
// untouched when setup fails. Close is graceful and idempotent; it also reports
// the first asynchronous write or rotation error so failures are not only
// swallowed by slog.
func Setup(cfg config.Logging) (io.Closer, error) {
	if cfg.Path == "" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
		return nopCloser{}, nil
	}
	c := cfg.WithDefaults()
	if c.MaxSizeMiB < 1 || c.MaxSizeMiB > 100 ||
		c.MaxBackups < 1 || c.MaxBackups > 20 ||
		c.MaxAgeDays < 1 || c.MaxAgeDays > 90 {
		return nil, fmt.Errorf("logging: invalid rotation limits")
	}
	if !filepath.IsAbs(c.Path) {
		return nil, fmt.Errorf("logging: path must be absolute")
	}
	w, err := newRotatingWriter(c.Path, int64(c.MaxSizeMiB)<<20, c.MaxBackups, time.Duration(c.MaxAgeDays)*24*time.Hour)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, nil)))
	return w, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
