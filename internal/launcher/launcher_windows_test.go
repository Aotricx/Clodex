//go:build windows

package launcher

import "testing"

func TestProcessGroupSysProcAttr(t *testing.T) {
	t.Parallel()

	if got := processGroupSysProcAttr(false); got != nil {
		t.Fatalf("processGroupSysProcAttr(false) = %#v, want nil", got)
	}
	got := processGroupSysProcAttr(true)
	if got == nil || got.CreationFlags != 0x00000200 {
		t.Fatalf("processGroupSysProcAttr(true) = %#v, want CreationFlags 0x00000200", got)
	}
}
