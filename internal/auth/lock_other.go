//go:build !unix && !windows

package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
)

func acquireExclusiveLock(ctx context.Context, lockPath string) (*authLock, error) {
	for {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			if chmodErr := file.Chmod(0o600); chmodErr != nil {
				name := file.Name()
				_ = file.Close()
				_ = os.Remove(name)
				return nil, fmt.Errorf("secure auth lock: %w", chmodErr)
			}
			return &authLock{file: file, removeOnClose: true}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("open auth lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			if err := waitLockRetry(ctx); err != nil {
				return nil, err
			}
		}
	}
}

func unlockAuthFile(*os.File) error { return nil }
