//go:build unix

package launcher

import "syscall"

func processGroupSysProcAttr(detach bool) *syscall.SysProcAttr {
	if !detach {
		return nil
	}
	return &syscall.SysProcAttr{Setpgid: true}
}
