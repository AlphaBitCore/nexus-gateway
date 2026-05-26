package wiring

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
)

func TestInitRedis_NoAddrs_ReturnsNil(t *testing.T) {
	// No addrs configured — function returns nil without connecting.
	cfg := &config.Config{}
	// cfg.Redis.Addrs is empty → returns nil.
	result := InitRedis(cfg, testLogger())
	if result != nil {
		// If somehow connected (shouldn't happen with empty addrs), close it.
		_ = result.Close()
		t.Error("expected nil client when no Redis addrs configured")
	}
}

// TestInitRedis_InvalidTLS_FactoryErrorReturnsNil exercises the redisfactory.New
// error branch (lines 31-34) by providing an invalid TLS CAFile path.
// redisfactory.New validates the CA file and returns an error when it doesn't exist.
func TestInitRedis_InvalidTLS_FactoryErrorReturnsNil(t *testing.T) {
	cfg := &config.Config{}
	cfg.Redis.Addrs = []string{"127.0.0.1:6379"}
	cfg.Redis.TLS.Enabled = true
	cfg.Redis.TLS.CAFile = "/nonexistent/path/ca.pem" // invalid CA → factory error

	result := InitRedis(cfg, testLogger())
	// Factory fails → returns nil (error logged).
	if result != nil {
		_ = result.Close()
		t.Error("expected nil when TLS CA file is invalid")
	}
}
