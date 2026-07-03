package proxy

import "testing"

func TestPerfPureForward_ReflectsPackageVar(t *testing.T) {
	orig := pureForward
	t.Cleanup(func() { pureForward = orig })

	pureForward = true
	if !PerfPureForward() {
		t.Error("PerfPureForward() = false, want true when pureForward set")
	}
	pureForward = false
	if PerfPureForward() {
		t.Error("PerfPureForward() = true, want false")
	}
}
