//go:build windows

package launcher

import "syscall"

const createNewProcessGroup = 0x00000200

func processGroupSysProcAttr(detach bool) *syscall.SysProcAttr {
	if !detach {
		return nil
	}
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}
