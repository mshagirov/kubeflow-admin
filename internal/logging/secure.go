package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ensurePrivateDir creates the log directory when absent and verifies that the
// immediate parent is a real directory owned by the effective user without
// group/world access. Ancestors are not required to be private: a dedicated
// 0700 directory inside a shared mount is the expected deployment shape.
//
// The mode is applied explicitly after creation because this library must not
// change the process umask.
func ensurePrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("logging: inspect directory %s: %w", dir, err)
		}
		if err := mkdirPrivateAll(dir); err != nil {
			return fmt.Errorf("logging: create private directory %s: %w", dir, err)
		}
		fi, err = os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("logging: inspect directory %s: %w", dir, err)
		}
	}
	return verifyPrivateDir(dir, fi)
}

// mkdirPrivateAll creates missing path components one at a time and applies
// 0700 to each component it creates. Doing this explicitly (rather than relying
// on MkdirAll's mode under an arbitrary process umask) keeps a very restrictive
// umask from making an intermediate directory untraversable.
func mkdirPrivateAll(dir string) error {
	dir = filepath.Clean(dir)
	var missing []string
	for p := dir; ; p = filepath.Dir(p) {
		if _, err := os.Lstat(p); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return err
		}
		if err := os.Chmod(missing[i], 0700); err != nil {
			return err
		}
	}
	return nil
}

func verifyPrivateDir(path string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("logging: directory %s must not be a symlink", path)
	}
	if !fi.IsDir() {
		return fmt.Errorf("logging: %s is not a directory", path)
	}
	if fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("logging: directory %s must not be group/world accessible", path)
	}
	if fi.Mode().Perm()&0700 != 0700 {
		return fmt.Errorf("logging: directory %s must be owner-readable/writable/searchable", path)
	}
	return verifyOwner(path, fi)
}

func verifyPrivateFile(path string, fi os.FileInfo) error {
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("logging: %s must not be a symlink", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("logging: %s must be a regular file", path)
	}
	if fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("logging: %s must not be group/world accessible", path)
	}
	return verifyOwner(path, fi)
}

func verifyOwner(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("logging: cannot verify ownership of %s", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("logging: %s must be owned by the effective user", path)
	}
	return nil
}

// validateActivePath performs the same active-file policy checks as
// openExistingPrivate without opening or creating anything. It lets Setup reject
// a bad active file before older backups are pruned.
func validateActivePath(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("logging: inspect %s: %w", path, err)
	}
	if err := verifyPrivateFile(path, fi); err != nil {
		return err
	}
	if fi.Mode().Perm()&0200 == 0 {
		return fmt.Errorf("logging: %s must be owner-writable", path)
	}
	return nil
}

// openExistingPrivate opens an active file without following a final symlink.
// The descriptor is re-checked after opening so a replacement between lstat and
// open cannot turn into an accepted symlink or less-private regular file.
func openExistingPrivate(path string) (*os.File, os.FileInfo, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyPrivateFile(path, fi); err != nil {
		return nil, nil, err
	}
	if fi.Mode().Perm()&0200 == 0 {
		return nil, nil, fmt.Errorf("logging: %s must be owner-writable", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: open %s: %w", path, err)
	}
	openFI, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("logging: inspect open file %s: %w", path, err)
	}
	if err := verifyPrivateFile(path, openFI); err != nil {
		f.Close()
		return nil, nil, err
	}
	// Normalize owner-only modes (for example 0200) to 0600. Group/world bits
	// were already rejected above, so this never broadens access.
	if openFI.Mode().Perm() != 0600 {
		if err := f.Chmod(0600); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("logging: secure %s: %w", path, err)
		}
		openFI, err = f.Stat()
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("logging: inspect open file %s: %w", path, err)
		}
	}
	return f, openFI, nil
}

// createPrivateFile creates an active file with O_NOFOLLOW|O_EXCL. O_EXCL makes
// a concurrent symlink or file substitution fail instead of being followed.
func createPrivateFile(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: create %s: %w", path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("logging: inspect open file %s: %w", path, err)
	}
	if err := verifyPrivateFile(path, fi); err != nil {
		f.Close()
		return nil, nil, err
	}
	// A restrictive process umask can only have removed owner bits; restore the
	// intended 0600 mode without touching process-global state.
	if fi.Mode().Perm() != 0600 {
		if err := f.Chmod(0600); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("logging: secure %s: %w", path, err)
		}
	}
	return f, fi, nil
}
