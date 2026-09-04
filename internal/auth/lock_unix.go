//go:build unix

package auth

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

func acquireExclusiveLock(ctx context.Context, lockPath string) (*authLock, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	file, err := openAuthLockFile(lockPath)
	if err != nil {
		return nil, err
	}

	locked := make(chan error, 1)
	go func() {
		locked <- syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	}()
	select {
	case <-ctx.Done():
		_ = file.Close()
		<-locked
		return nil, ctx.Err()
	case err := <-locked:
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("lock auth: %w", err)
		}
		return &authLock{file: file}, nil
	}
}

func unlockAuthFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock auth: %w", err)
	}
	return nil
}
