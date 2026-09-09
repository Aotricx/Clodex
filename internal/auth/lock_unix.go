//go:build unix

package auth

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

// acquireExclusiveLock polls LOCK_EX|LOCK_NB so a canceled waiter can return
// without Close()ing a file descriptor that another goroutine has blocked in
// Flock. Darwin close(2) waits for that flock to finish.
func acquireExclusiveLock(ctx context.Context, lockPath string) (*authLock, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	file, err := openAuthLockFile(lockPath)
	if err != nil {
		return nil, err
	}

	for {
		if err := contextError(ctx); err != nil {
			_ = file.Close()
			return nil, err
		}
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &authLock{file: file}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = file.Close()
			return nil, fmt.Errorf("lock auth: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		default:
			if err := waitLockRetry(ctx); err != nil {
				_ = file.Close()
				return nil, err
			}
		}
	}
}

func unlockAuthFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock auth: %w", err)
	}
	return nil
}
