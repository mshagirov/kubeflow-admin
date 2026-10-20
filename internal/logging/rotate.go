package logging

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rotatingWriter is a size-bounded writer for the active JSON log file. It is
// safe for concurrent Write/Close calls. It never buffers a whole record and
// never copies an oversized payload: a write above the record limit is replaced
// by a small fixed notice containing only the rejected size.
type rotatingWriter struct {
	mu sync.Mutex

	path           string
	maxBytes       int64
	maxBackups     int
	maxAge         time.Duration
	maxRecordBytes int
	now            func() time.Time

	f      *os.File
	size   int64
	err    error
	closed bool
}

func newRotatingWriter(path string, maxBytes int64, maxBackups int, maxAge time.Duration) (*rotatingWriter, error) {
	if path == "" || maxBytes <= 0 || maxBackups < 1 || maxAge < 0 {
		return nil, fmt.Errorf("logging: invalid rotating writer limits")
	}
	limit := int64(defaultMaxRecordBytes)
	if maxBytes < limit {
		limit = maxBytes
	}
	if limit < 1 {
		limit = 1
	}
	w := &rotatingWriter{
		path:           path,
		maxBytes:       maxBytes,
		maxBackups:     maxBackups,
		maxAge:         maxAge,
		maxRecordBytes: int(limit),
		now:            time.Now,
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := validateActivePath(path); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && info.Size() > maxBytes {
		return nil, fmt.Errorf("logging: existing active file exceeds configured size limit; archive it before enabling bounded rotation")
	}
	if err := w.cleanupBackupsLocked(); err != nil {
		return nil, err
	}
	if err := w.openActiveLocked(); err != nil {
		return nil, err
	}
	if w.size > w.maxBytes {
		w.f.Close()
		return nil, fmt.Errorf("logging: existing active file exceeds configured size limit; archive it before enabling bounded rotation")
	}
	if w.size >= w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			if w.f != nil {
				w.f.Close()
				w.f = nil
			}
			return nil, err
		}
	}
	return w, nil
}

// Write implements io.Writer. Returning the full slice length after writing a
// bounded replacement notice keeps slog from retrying; the original oversized
// content is intentionally discarded rather than copied or persisted.
func (w *rotatingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, os.ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if len(p) > w.maxRecordBytes {
		if _, err := w.writeLocked(w.rejectionNotice(len(p))); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	n, err := w.writeLocked(p)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

func (w *rotatingWriter) writeLocked(p []byte) (int, error) {
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.f == nil {
		if err := w.openActiveLocked(); err != nil {
			w.err = err
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			w.err = err
			return 0, err
		}
	}
	if int64(len(p)) > w.maxBytes {
		p = p[:w.maxBytes]
	}
	n, err := w.f.Write(p)
	if n > 0 {
		w.size += int64(n)
	}
	if err != nil {
		err = fmt.Errorf("logging: write %s: %w", w.path, err)
		w.err = err
		return n, err
	}
	if n != len(p) {
		err = fmt.Errorf("logging: write %s: %w", w.path, io.ErrShortWrite)
		w.err = err
		return n, err
	}
	return n, nil
}

func (w *rotatingWriter) rejectionNotice(size int) []byte {
	t := w.now().UTC().Format(time.RFC3339Nano)
	return []byte(fmt.Sprintf(`{"time":%q,"level":"WARN","msg":"log record rejected: exceeds size limit","size":%d}`+"\n", t, size))
}

func (w *rotatingWriter) openActiveLocked() error {
	f, fi, err := openExistingPrivate(w.path)
	if err == nil {
		w.f = f
		w.size = fi.Size()
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	f, fi, err = createPrivateFile(w.path)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// A racing creator appeared between lstat and O_EXCL. Re-open only
			// through the same private-file checks; otherwise fail closed.
			f, fi, err = openExistingPrivate(w.path)
			if err != nil {
				return err
			}
			w.f = f
			w.size = fi.Size()
			return nil
		}
		return err
	}
	w.f = f
	w.size = fi.Size()
	return nil
}

func (w *rotatingWriter) createActiveLocked() error {
	f, fi, err := createPrivateFile(w.path)
	if err != nil {
		return err
	}
	w.f = f
	w.size = fi.Size()
	return nil
}

// rotateLocked closes and renames the active file, shifts numbered backups and
// opens a fresh private active file. If any step fails, the deferred recovery
// makes a best effort to re-open a usable active file before returning the
// original error; writeLocked then records the failure and stops the sink.
func (w *rotatingWriter) rotateLocked() (err error) {
	if w.f != nil {
		closeErr := w.f.Close()
		w.f = nil
		w.size = 0
		if closeErr != nil {
			return fmt.Errorf("logging: close rotated file %s: %w", w.path, closeErr)
		}
	}
	w.size = 0
	defer func() {
		if w.f != nil {
			return
		}
		if reopenErr := w.openActiveLocked(); reopenErr != nil {
			if err == nil {
				err = reopenErr
			} else {
				err = errors.Join(err, reopenErr)
			}
		}
	}()

	if cleanupErr := w.cleanupBackupsLocked(); cleanupErr != nil {
		return cleanupErr
	}
	// Drop the oldest retained backup before shifting so the resulting count is
	// exactly maxBackups and the rename does not depend on overwrite semantics.
	if removeErr := w.removeBackupLocked(w.maxBackups); removeErr != nil {
		return removeErr
	}
	if shiftErr := w.shiftBackupsLocked(); shiftErr != nil {
		return shiftErr
	}
	if renameErr := os.Rename(w.path, w.backupPath(1)); renameErr != nil && !os.IsNotExist(renameErr) {
		return fmt.Errorf("logging: rotate %s: %w", w.path, renameErr)
	}
	if createErr := w.createActiveLocked(); createErr != nil {
		return createErr
	}
	return nil
}

func (w *rotatingWriter) shiftBackupsLocked() error {
	for i := w.maxBackups - 1; i >= 1; i-- {
		src := w.backupPath(i)
		dst := w.backupPath(i + 1)
		if err := os.Rename(src, dst); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("logging: rotate backup %s: %w", src, err)
		}
	}
	return nil
}

// cleanupBackupsLocked validates every numbered backup before touching it, so a
// planted symlink, non-regular file, foreign owner or group/world-accessible
// backup fails closed instead of being followed. It removes backups beyond the
// configured count and backups older than maxAge (age is file mtime).
func (w *rotatingWriter) cleanupBackupsLocked() error {
	dir := filepath.Dir(w.path)
	base := filepath.Base(w.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("logging: list log directory %s: %w", dir, err)
	}
	cutoff := w.now().Add(-w.maxAge)
	for _, entry := range entries {
		index, ok := backupIndex(base, entry.Name())
		if !ok {
			continue
		}
		p := filepath.Join(dir, entry.Name())
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("logging: inspect backup %s: %w", p, err)
		}
		if err := verifyPrivateFile(p, fi); err != nil {
			return err
		}
		if index > w.maxBackups || fi.ModTime().Before(cutoff) {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("logging: remove backup %s: %w", p, err)
			}
		} else if fi.Size() > w.maxBytes {
			return fmt.Errorf("logging: retained backup exceeds configured size limit; archive it before enabling bounded rotation")
		}
	}
	return nil
}

func (w *rotatingWriter) removeBackupLocked(index int) error {
	p := w.backupPath(index)
	fi, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("logging: inspect backup %s: %w", p, err)
	}
	if err := verifyPrivateFile(p, fi); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logging: remove backup %s: %w", p, err)
	}
	return nil
}

func (w *rotatingWriter) backupPath(index int) string {
	return fmt.Sprintf("%s.%d", w.path, index)
}

// backupIndex recognizes only canonical positive decimal suffixes, so unrelated
// files such as "admin.log.old" are never treated as application backups.
func backupIndex(base, name string) (int, bool) {
	prefix := base + "."
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == "" {
		return 0, false
	}
	index, err := strconv.Atoi(suffix)
	if err != nil || index < 1 || strconv.Itoa(index) != suffix {
		return 0, false
	}
	return index, true
}

// Err reports the first asynchronous write or rotation failure. Setup returns
// io.Closer; callers that want to inspect post-setup failures may type-assert
// the closer to interface{ Err() error }. Close also returns this error.
func (w *rotatingWriter) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Close stops the sink gracefully and is idempotent. It reports a prior write or
// rotation failure together with any close error, without including log record
// contents.
func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	var errs []error
	if w.err != nil {
		errs = append(errs, w.err)
		w.err = nil
	}
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("logging: close %s: %w", w.path, err))
		}
		w.f = nil
	}
	return errors.Join(errs...)
}
