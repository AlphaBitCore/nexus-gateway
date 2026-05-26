package enrollment

// SetExecCommandStart replaces the package-private exec.Start seam used
// by openBrowser. Tests outside this package call this to neutralise the
// real shell-out so running `go test` never spawns a browser tab when
// exercising the OpenBrowser==nil default-fallback path. Returns a
// restore func; callers should `t.Cleanup(restore)`.
func SetExecCommandStart(fn func(name string, args ...string) error) func() {
	orig := ssoExecCommandStart
	ssoExecCommandStart = fn
	return func() { ssoExecCommandStart = orig }
}
