package auth

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
)

// authLock holds an advisory exclusive lock on the sibling lock file next to
// a resolved auth.json path. Close/Unlock releases it; the lock file itself
// is left in place so Codex CLI readers of auth.json are unaffected.
//
// lockAuth itself is not reentrant. Darwin flock and Windows LockFileEx are
// per-handle, so a nested lockAuth from the same goroutine blocks. UpdateAtomically
// skips lockAuth when the current goroutine already holds it (refreshIf).
type authLock struct {
	file          *os.File
	removeOnClose bool
	lockPath      string
	ownerGID      uint64
}

type authLockHolderKey struct {
	path string
	gid  uint64
}

var (
	authLockHoldersMu sync.Mutex
	authLockHolders   = map[authLockHolderKey]struct{}{}
)

func lockAuth(ctx context.Context, authPath string) (*authLock, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	writePath, err := resolveAuthWritePath(authPath)
	if err != nil {
		return nil, err
	}
	lockPath := writePath + ".lock"
	held, err := acquireExclusiveLock(ctx, lockPath)
	if err != nil {
		return nil, err
	}
	gid := goroutineID()
	authLockHoldersMu.Lock()
	authLockHolders[authLockHolderKey{path: lockPath, gid: gid}] = struct{}{}
	authLockHoldersMu.Unlock()
	held.lockPath = lockPath
	held.ownerGID = gid
	return held, nil
}

func authLockHeldByCurrentGoroutine(authPath string) bool {
	writePath, err := resolveAuthWritePath(authPath)
	if err != nil {
		return false
	}
	key := authLockHolderKey{path: writePath + ".lock", gid: goroutineID()}
	authLockHoldersMu.Lock()
	defer authLockHoldersMu.Unlock()
	_, ok := authLockHolders[key]
	return ok
}

func (l *authLock) Unlock() error {
	if l == nil {
		return nil
	}
	if l.lockPath != "" {
		authLockHoldersMu.Lock()
		delete(authLockHolders, authLockHolderKey{path: l.lockPath, gid: l.ownerGID})
		authLockHoldersMu.Unlock()
		l.lockPath = ""
	}
	if l.file == nil {
		return nil
	}
	name := l.file.Name()
	unlockErr := unlockAuthFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	var removeErr error
	if l.removeOnClose {
		removeErr = os.Remove(name)
	}
	if unlockErr != nil {
		return unlockErr
	}
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

func openAuthLockFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open auth lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure auth lock: %w", err)
	}
	return file, nil
}

func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	s := buf[:n]
	const prefix = "goroutine "
	if len(s) < len(prefix) {
		return 0
	}
	s = s[len(prefix):]
	var id uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + uint64(c-'0')
	}
	return id
}
