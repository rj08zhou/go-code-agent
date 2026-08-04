// Package store provides persistence primitives.
package store

import (
	"os"
	"path/filepath"
)

// AtomicWrite writes data to path atomically via tmp + rename + fsync parent dir.
func AtomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicReplace(path, data, mode)
}

// EnsurePrivateDir creates dir (and parents) owner-only and tightens the
// permissions of a pre-existing dir. Chmod is applied explicitly because
// MkdirAll modes are narrowed by umask but never widened, while legacy
// directories may have been created 0755.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// AtomicWritePrivate is AtomicWrite for agent-private state (session metadata,
// histories, memories): parent dirs 0700, file 0600, tightening pre-existing
// permissive modes. Never use it for files in the user's workspace.
func AtomicWritePrivate(path string, data []byte) error {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	return atomicReplace(path, data, 0o600)
}

// atomicReplace writes one fully prepared target using the caller-selected
// mode. Directory creation and target-mode selection deliberately remain with
// the public wrappers because workspace and private state have different rules.
func atomicReplace(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncParentDir(filepath.Dir(path))
	return nil
}

// syncParentDir makes a completed rename durable when the underlying
// filesystem supports directory syncing. Preserve the existing best-effort
// behavior: failure to open or sync the directory does not fail the write.
func syncParentDir(dirPath string) {
	dir, err := os.Open(dirPath)
	if err != nil {
		return
	}
	defer dir.Close()
	_ = dir.Sync()
}

// OpenPrivateAppend opens an append-only agent-private file with owner-only
// permissions, creating parent dirs 0700 and tightening a pre-existing file.
func OpenPrivateAppend(path string) (*os.File, error) {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
