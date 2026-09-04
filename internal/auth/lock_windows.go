//go:build windows

package auth

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	lockfileFailImmediately = 1
	lockfileExclusiveLock   = 2
	errorLockViolation      = syscall.Errno(33)
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

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
		err := lockFileEx(file)
		if err == nil {
			return &authLock{file: file}, nil
		}
		if err != errorLockViolation && err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = file.Close()
			return nil, fmt.Errorf("lock auth: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func unlockAuthFile(file *os.File) error {
	if err := unlockFileEx(file); err != nil {
		return fmt.Errorf("unlock auth: %w", err)
	}
	return nil
}

func lockFileEx(file *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, err := procLockFileEx.Call(
		uintptr(file.Fd()),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		if err != syscall.Errno(0) {
			return err
		}
		return syscall.EINVAL
	}
	return nil
}

func unlockFileEx(file *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, err := procUnlockFileEx.Call(
		uintptr(file.Fd()),
		0,
		uintptr(^uint32(0)),
		uintptr(^uint32(0)),
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		if err != syscall.Errno(0) {
			return err
		}
		return syscall.EINVAL
	}
	return nil
}
