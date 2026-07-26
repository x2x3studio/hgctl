// Package fsx holds the filesystem primitives hgctl relies on to survive being
// killed: atomic writes, JSON persistence built on them, advisory locks, and the
// schema-version probe that guards every persisted file.
//
// A leaf package - it depends on nothing else in this module.
//
// The reason it exists as one package rather than three: everything here answers
// the same question, which is "what happens if this process dies right now". An
// endpoint is a scheduled job on a laptop that sleeps, so that is not a rare
// case, and a half-written state file or a lock nobody releases costs more than
// the work that was interrupted.
package fsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// ErrUnsupportedSchema reports a persisted file written by a version of hgctl
// that this one cannot read.
var ErrUnsupportedSchema = errors.New("unsupported persisted schema version")

// WriteAtomic writes content to path via a temp file and a rename, so a reader
// sees either the old bytes or the new ones and never a truncated file.
func WriteAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".hgctl-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return err
	}
	// Sync before rename: rename is atomic in the directory, but without this the
	// content can still be lost to a power failure while the name already points
	// at it.
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// ReadJSON decodes path into dst.
func ReadJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// WriteJSON encodes value to path atomically.
func WriteJSON(path string, value any, mode os.FileMode) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteAtomic(path, b, mode)
}

// ProbeSchema reads path and reports whether its schema version needs migrating,
// without writing anything. A missing file is not an error - there is nothing to
// be incompatible with yet.
func ProbeSchema(path string, dst any, version *int, current int) (bool, error) {
	if err := ReadJSON(path, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return needsMigration(path, *version, current)
}

// MigrateSchema advances *version to current, reporting whether it moved.
//
// Version 0 means "written before schema versions existed" and is migrated
// silently; anything else unrecognised is an error rather than a guess, because
// reading a newer file with older code is how persisted state gets corrupted.
func MigrateSchema(path string, version *int, current int) (bool, error) {
	moved, err := needsMigration(path, *version, current)
	if err != nil || !moved {
		return false, err
	}
	*version = current
	return true, nil
}

func needsMigration(path string, version, current int) (bool, error) {
	switch version {
	case 0:
		if current != 1 {
			return false, fmt.Errorf("%w: %s requires an explicit migration from the legacy schema to %d", ErrUnsupportedSchema, path, current)
		}
		return true, nil
	case current:
		return false, nil
	default:
		return false, fmt.Errorf("%w: %s has unsupported schema_version %d; current is %d", ErrUnsupportedSchema, path, version, current)
	}
}

// WithLock runs fn holding an advisory lock on path, and does NOTHING if the
// lock is already held.
//
// Silence is the point: the caller is the once-a-minute scheduler, and a tick
// that arrives while the previous one is still working should skip, not queue up
// behind it. The lock is released by the OS on process death, so a killed run
// never needs manual cleanup.
func WithLock(path string, fn func() error) error {
	f, err := openLock(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// WithLockWait is WithLock for callers who must not be skipped - an operator
// running a command by hand - and waits until ctx expires.
func WithLockWait(ctx context.Context, path string, fn func() error) error {
	f, err := openLock(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for lock %s: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func openLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

// Canonical resolves path to an absolute, symlink-free, cleaned form so two
// spellings of the same directory compare equal. Ownership checks depend on
// that: /Users/x/vault and a /tmp symlink to it must not look like two vaults.
func Canonical(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// Bound truncates value to at most limit BYTES without splitting a rune, and
// replaces invalid UTF-8 first. Callers bound text that reaches frontmatter and
// error messages, where a split rune would corrupt the file that carries it.
func Bound(value string, limit int) string {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= limit {
		return value
	}
	b := []byte(value)[:limit]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
