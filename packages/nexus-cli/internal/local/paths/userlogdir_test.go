package paths

import (
	"runtime"
	"strings"
	"testing"
)

// TestUserLogDir_HappyAndError exercises the build-tagged userLogDir for the
// current GOOS directly: the resolved happy-path location and the error branch
// when the home/appdata source is unavailable. Calling userLogDir directly
// (rather than only through DefaultPaths) covers the error return that
// DefaultPaths short-circuits past when os.UserConfigDir fails first.
func TestUserLogDir_HappyAndError(t *testing.T) {
	// Happy path: a real dir is resolved and ends in our app segment.
	dir, err := userLogDir()
	if err != nil {
		t.Fatalf("userLogDir happy path: %v", err)
	}
	if !strings.Contains(dir, "nexus") {
		t.Errorf("userLogDir = %q, want it to contain 'nexus'", dir)
	}

	// Error path: clear the source the current GOOS depends on.
	switch runtime.GOOS {
	case "darwin", "linux":
		t.Setenv("HOME", "")
		t.Setenv("XDG_STATE_HOME", "")
		if _, err := userLogDir(); err == nil {
			t.Error("expected userLogDir error with HOME unset, got nil")
		}
	case "windows":
		// userLogDir falls back to os.UserCacheDir() when LocalAppData is
		// empty; both read LocalAppData, so clearing it forces the fallback's
		// error.
		t.Setenv("LocalAppData", "")
		// os.UserCacheDir on windows also reads LocalAppData, so it errors too.
		if _, err := userLogDir(); err == nil {
			t.Error("expected userLogDir error with LocalAppData unset, got nil")
		}
	default:
		t.Skipf("no env-clearing recipe for GOOS %q", runtime.GOOS)
	}
}
