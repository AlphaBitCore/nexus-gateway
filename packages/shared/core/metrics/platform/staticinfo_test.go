package platform

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestCaptureStaticInfoIncludesIdentity(t *testing.T) {
	info := CaptureStaticInfo(BuildInfo{
		Service:      "nexus-hub",
		BuildVersion: "v1.4.2@abc1234",
		StartTime:    "2026-04-26T08:00:00Z",
	})
	if info.Hostname == "" {
		t.Error("hostname not captured")
	}
	if info.OS == "" {
		t.Error("os not captured")
	}
	if info.CPUCores <= 0 {
		t.Errorf("cpuCores = %d, want >0", info.CPUCores)
	}
	if info.ServiceVersion != "nexus-hub/v1.4.2@abc1234" {
		t.Errorf("serviceVersion mismatch: %q", info.ServiceVersion)
	}
	// The three build fields are resolved inside CaptureStaticInfo now, so a
	// caller cannot report an empty sha by forgetting to fill one in.
	// Seven hex digits because that is git's own abbreviation floor and the
	// shortest thing shaFromTaggedVersion will accept. It used to be six,
	// which no `git rev-parse --short` emits by default — a stand-in short
	// enough that it is now rejected, which is the point of the validation
	// rather than an accident of it.
	if info.BuildSHA != "abc1234" {
		t.Errorf("buildSHA = %q, want the sha carried by the build version", info.BuildSHA)
	}
}

// The reported staticInfo carries the URLs peers resolve via the Hub
// (publicUrl for external clients, privateUrl for service-to-service):
// BuildInfo.PublicURL/PrivateURL must land verbatim on StaticInfo.
func TestCaptureStaticInfoCarriesURLs(t *testing.T) {
	info := CaptureStaticInfo(BuildInfo{
		PublicURL:  "https://gw.example.com",
		PrivateURL: "http://10.0.0.5:3050",
	})
	if info.PublicURL != "https://gw.example.com" {
		t.Errorf("PublicURL = %q; want the build-info public URL", info.PublicURL)
	}
	if info.PrivateURL != "http://10.0.0.5:3050" {
		t.Errorf("PrivateURL = %q; want the build-info private URL", info.PrivateURL)
	}
}

// stubUDPConn is a minimal net.Conn whose LocalAddr returns a *net.UDPAddr
// with an injectable IP, so EffectivePrivateURL's auto-derivation can be
// exercised deterministically without real network access.
type stubUDPConn struct{ ip net.IP }

func (c *stubUDPConn) Read(_ []byte) (int, error)         { return 0, nil }
func (c *stubUDPConn) Write(_ []byte) (int, error)        { return 0, nil }
func (c *stubUDPConn) Close() error                       { return nil }
func (c *stubUDPConn) LocalAddr() net.Addr                { return &net.UDPAddr{IP: c.ip, Port: 33333} }
func (c *stubUDPConn) RemoteAddr() net.Addr               { return &net.UDPAddr{} }
func (c *stubUDPConn) SetDeadline(_ time.Time) error      { return nil }
func (c *stubUDPConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *stubUDPConn) SetWriteDeadline(_ time.Time) error { return nil }

// stubOutboundIP redirects primaryOutboundIP to a fixed local IP for the
// duration of the test.
func stubOutboundIP(t *testing.T, ip net.IP) {
	t.Helper()
	orig := dialContextFn
	t.Cleanup(func() { dialContextFn = orig })
	dialContextFn = func(_ context.Context, _, _ string) (net.Conn, error) {
		return &stubUDPConn{ip: ip}, nil
	}
}

func TestEffectivePrivateURL(t *testing.T) {
	t.Run("override wins over derivation", func(t *testing.T) {
		stubOutboundIP(t, net.IPv4(10, 0, 0, 7))
		got := EffectivePrivateURL("https://internal.example.com:9443", "", 3050)
		if got != "https://internal.example.com:9443" {
			t.Fatalf("EffectivePrivateURL = %q; want the override verbatim", got)
		}
	})

	t.Run("override trailing slash trimmed", func(t *testing.T) {
		got := EffectivePrivateURL("https://internal.example.com/", "", 3050)
		if got != "https://internal.example.com" {
			t.Fatalf("EffectivePrivateURL = %q; want trailing slash trimmed", got)
		}
	})

	t.Run("no override derives from primary IPv4 and port", func(t *testing.T) {
		stubOutboundIP(t, net.IPv4(10, 0, 0, 7))
		got := EffectivePrivateURL("", "", 3050)
		if got != "http://10.0.0.7:3050" {
			t.Fatalf("EffectivePrivateURL = %q; want http://10.0.0.7:3050", got)
		}
	})

	t.Run("IPv6 primary address is bracketed", func(t *testing.T) {
		stubOutboundIP(t, net.ParseIP("fd00::5"))
		got := EffectivePrivateURL("", "", 3050)
		if got != "http://[fd00::5]:3050" {
			t.Fatalf("EffectivePrivateURL = %q; want bracketed IPv6 host", got)
		}
	})

	t.Run("no primary IP yields empty (not-yet-reported)", func(t *testing.T) {
		orig := dialContextFn
		t.Cleanup(func() { dialContextFn = orig })
		dialContextFn = func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("no route")
		}
		if got := EffectivePrivateURL("", "", 3050); got != "" {
			t.Fatalf("EffectivePrivateURL = %q; want empty when the primary IP is unknown", got)
		}
	})

	t.Run("non-positive port yields empty", func(t *testing.T) {
		stubOutboundIP(t, net.IPv4(10, 0, 0, 7))
		for _, port := range []int{0, -1} {
			if got := EffectivePrivateURL("", "", port); got != "" {
				t.Fatalf("EffectivePrivateURL(port=%d) = %q; want empty", port, got)
			}
		}
	})

	t.Run("loopback bind host is advertised, not the primary IP", func(t *testing.T) {
		// A socket bound to 127.0.0.1 refuses dials to the machine's LAN IP
		// even from the same host — the single-box appliance binds loopback
		// behind nginx, so the advertised private URL MUST be loopback too.
		stubOutboundIP(t, net.IPv4(10, 0, 0, 7))
		if got := EffectivePrivateURL("", "127.0.0.1", 3050); got != "http://127.0.0.1:3050" {
			t.Fatalf("EffectivePrivateURL = %q; want the loopback bind host advertised", got)
		}
	})

	t.Run("specific non-loopback bind host is advertised verbatim", func(t *testing.T) {
		stubOutboundIP(t, net.IPv4(10, 0, 0, 7))
		if got := EffectivePrivateURL("", "192.168.1.20", 3050); got != "http://192.168.1.20:3050" {
			t.Fatalf("EffectivePrivateURL = %q; want the bind host advertised", got)
		}
	})

	t.Run("wildcard binds fall back to the primary IP", func(t *testing.T) {
		stubOutboundIP(t, net.IPv4(10, 0, 0, 7))
		for _, wild := range []string{"", "0.0.0.0", "::", "[::]"} {
			if got := EffectivePrivateURL("", wild, 3050); got != "http://10.0.0.7:3050" {
				t.Fatalf("EffectivePrivateURL(bind=%q) = %q; want primary-IP derivation", wild, got)
			}
		}
	})

	t.Run("IPv6 bind host is bracketed", func(t *testing.T) {
		if got := EffectivePrivateURL("", "fd00::9", 3050); got != "http://[fd00::9]:3050" {
			t.Fatalf("EffectivePrivateURL = %q; want bracketed IPv6 bind host", got)
		}
	})
}
