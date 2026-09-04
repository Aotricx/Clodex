//go:build unix

package launcher

import "testing"

func TestProcessGroupSysProcAttr(t *testing.T) {
	t.Parallel()

	if got := processGroupSysProcAttr(false); got != nil {
		t.Fatalf("processGroupSysProcAttr(false) = %#v, want nil", got)
	}
	got := processGroupSysProcAttr(true)
	if got == nil || !got.Setpgid {
		t.Fatalf("processGroupSysProcAttr(true) = %#v, want Setpgid true", got)
	}
}
