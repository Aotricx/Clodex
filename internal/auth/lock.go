package auth

import (
	"context"
	"fmt"
	"os"
)

// authLock holds an advisory exclusive lock on the sibling lock file next to
// a resolved auth.json path. Close/Unlock releases it; the lock file itself
// is left in place so Codex CLI readers of auth.json are unaffected.
type authLock struct {
	file          *os.File
	removeOnClose bool
}

func lockAuth(ctx context.Context, authPath string) (*authLock, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	writePath, err := resolveAuthWritePath(authPath)
	if err != nil {
		return nil, err
	}
	return acquireExclusiveLock(ctx, writePath+".lock")
}

func (l *authLock) Unlock() error {
	if l == nil || l.file == nil {
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
