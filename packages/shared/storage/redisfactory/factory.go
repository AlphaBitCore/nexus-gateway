package redisfactory

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// Mode selects the deployment topology of the Redis backend. The yaml field
// is `mode:`; the env override is REDIS_MODE.
type Mode string

const (
	// ModeStandalone selects a single Redis instance (default).
	ModeStandalone Mode = "standalone"
	// ModeSentinel selects sentinel-managed failover via
	// [redis.FailoverOptions]. Requires [SentinelConfig.MasterName].
	ModeSentinel Mode = "sentinel"
	// ModeCluster selects a sharded Redis cluster via
	// [redis.ClusterOptions]. Requires at least one address in [Config.Addrs].
	ModeCluster Mode = "cluster"
)

// Config is the universal Redis configuration consumed by every Nexus Gateway
// service. yaml field names are documented inline; the matching env variable
// is REDIS_<UPPER_SNAKE> per the LoadEnv contract (see env.go).
type Config struct {
	// Mode is one of "standalone" | "sentinel" | "cluster". Defaults to
	// "standalone" when blank.
	Mode Mode `yaml:"mode"`
	// Addrs is the list of host:port endpoints. Standalone uses Addrs[0];
	// sentinel uses the full list as the sentinel set; cluster uses all
	// entries as cluster seeds. Required.
	Addrs []string `yaml:"addrs"`
	// Username is the ACL username (Redis 6+). Leave blank for the legacy
	// AUTH-only password flow used by pre-6 Redis or Redis without ACLs.
	Username string `yaml:"username"`
	// Password is the AUTH password. Secret — overridable via REDIS_PASSWORD.
	Password string `yaml:"password"`
	// DB selects the logical database (standalone + sentinel). Cluster
	// ignores this field.
	DB int `yaml:"db"`

	// Sentinel configures sentinel-specific knobs.
	Sentinel SentinelConfig `yaml:"sentinel"`
	// Cluster configures cluster-specific knobs.
	Cluster ClusterConfig `yaml:"cluster"`
	// TLS configures the optional TLS / mTLS handshake.
	TLS TLSConfig `yaml:"tls"`

	// PoolSize caps the number of socket connections per node. Zero
	// defers to the go-redis default (10 * GOMAXPROCS).
	PoolSize int `yaml:"poolSize"`
	// MinIdleConns is the minimum number of idle pooled connections kept
	// warm. Zero disables the warming behaviour.
	MinIdleConns int `yaml:"minIdleConns"`
	// MaxRetries is the per-command retry budget. Negative disables retries
	// entirely; zero defers to the go-redis default (3).
	MaxRetries int `yaml:"maxRetries"`

	// DialTimeout caps the TCP connect duration per Dial.
	DialTimeout time.Duration `yaml:"dialTimeout"`
	// ReadTimeout caps the per-command read duration. Zero defers to the
	// go-redis default (3s).
	ReadTimeout time.Duration `yaml:"readTimeout"`
	// WriteTimeout caps the per-command write duration. Zero defaults to
	// ReadTimeout.
	WriteTimeout time.Duration `yaml:"writeTimeout"`
	// PoolTimeout caps how long a caller waits for a free pooled
	// connection before the operation fails with ErrPoolTimeout.
	PoolTimeout time.Duration `yaml:"poolTimeout"`

	// NoPing skips the startup readiness PING. By default New issues a PING
	// after building the client and returns an error if the endpoint is
	// unreachable, so a misconfigured / down Redis fails loudly at startup
	// rather than on the first cache call. Set true only where no real Redis
	// is reachable at construction time (unit tests that exercise config
	// shaping without a live endpoint). Not a yaml field — test-only.
	NoPing bool `yaml:"-"`

	// PingTimeout bounds the startup PING. Zero defers to DefaultPingTimeout.
	PingTimeout time.Duration `yaml:"pingTimeout"`
}

// DefaultPingTimeout bounds the startup readiness PING when Config.PingTimeout
// is unset, so a black-holed endpoint fails startup in bounded time instead of
// hanging on the dial.
const DefaultPingTimeout = 5 * time.Second

// SentinelConfig holds the knobs specific to sentinel-managed failover.
type SentinelConfig struct {
	// MasterName names the sentinel master set. Required in sentinel mode.
	MasterName string `yaml:"masterName"`
	// Username is the ACL username used to authenticate against the
	// sentinel daemons themselves (independent of [Config.Username] which
	// authenticates against the data-plane Redis instances).
	Username string `yaml:"username"`
	// Password is the sentinel-daemon AUTH password.
	Password string `yaml:"password"`
}

// ClusterConfig holds the knobs specific to a sharded Redis cluster.
type ClusterConfig struct {
	// MaxRedirects is the MOVED/ASK redirect budget per command. Zero
	// defers to the go-redis default (3).
	MaxRedirects int `yaml:"maxRedirects"`
	// RouteRandomly spreads read traffic to random replicas instead of
	// always hitting the master.
	RouteRandomly bool `yaml:"routeRandomly"`
	// ReadOnly enables the cluster read-only mode (queries go to slaves).
	ReadOnly bool `yaml:"readOnly"`
}

// TLSConfig holds the optional TLS / mTLS configuration for the Redis
// connection. CA, client cert, and client key paths are filesystem paths
// resolved at construction time.
type TLSConfig struct {
	// Enabled gates TLS entirely. When false the other fields are ignored.
	Enabled bool `yaml:"enabled"`
	// InsecureSkipVerify disables server certificate verification. Dev/test
	// only — every prod deployment must leave this false.
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
	// CAFile is a PEM-encoded CA bundle used to verify the server
	// certificate. Leave blank to use the system trust store.
	CAFile string `yaml:"caFile"`
	// CertFile is the PEM-encoded client certificate used for mTLS.
	// Pair with KeyFile; either both or neither must be set.
	CertFile string `yaml:"certFile"`
	// KeyFile is the PEM-encoded client private key used for mTLS.
	KeyFile string `yaml:"keyFile"`
	// ServerName overrides the SNI / verify hostname presented to the
	// server. Leave blank to use the host portion of [Config.Addrs][0].
	ServerName string `yaml:"serverName"`
}

// New builds a [redis.UniversalClient] from yamlCfg + env, applying env-wins
// precedence (L3 > L2). The returned client must be closed by the caller.
//
// New issues a startup readiness PING by default (bounded by
// Config.PingTimeout / DefaultPingTimeout) and returns an error if the
// endpoint is unreachable — a down or misconfigured Redis then fails loudly at
// service startup instead of surfacing as a confusing error on the first cache
// call. Set Config.NoPing to skip the PING (unit tests with no live endpoint).
//
// Returns an error when the merged Config is invalid (unknown mode, missing
// sentinel master name in sentinel mode, missing addrs, or malformed mTLS), or
// when the startup PING fails.
func New(yamlCfg Config, env Env, logger *slog.Logger) (redis.UniversalClient, error) {
	cfg := mergeEnv(yamlCfg, env)
	if cfg.Mode == "" {
		cfg.Mode = ModeStandalone
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	tlsCfg, err := buildTLS(cfg.TLS, cfg.Addrs)
	if err != nil {
		return nil, fmt.Errorf("redis tls: %w", err)
	}

	client, err := buildClient(cfg, tlsCfg)
	if err != nil {
		return nil, err
	}

	if !yamlCfg.NoPing {
		pingTimeout := yamlCfg.PingTimeout
		if pingTimeout <= 0 {
			pingTimeout = DefaultPingTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		defer cancel()
		if perr := client.Ping(ctx).Err(); perr != nil {
			// Close the pool we just opened so a failed startup doesn't leak
			// the dialer goroutines / sockets.
			_ = client.Close()
			return nil, fmt.Errorf("redis: startup ping failed (mode=%s addrs=%v): %w", cfg.Mode, cfg.Addrs, perr)
		}
	}
	return client, nil
}

// buildClient constructs the mode-specific UniversalClient. It does not ping;
// New handles the startup readiness check.
func buildClient(cfg Config, tlsCfg *tls.Config) (redis.UniversalClient, error) {
	switch cfg.Mode {
	case ModeStandalone:
		return redis.NewClient(&redis.Options{
			Addr:         cfg.Addrs[0],
			Username:     cfg.Username,
			Password:     cfg.Password,
			DB:           cfg.DB,
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
			MaxRetries:   cfg.MaxRetries,
			DialTimeout:  cfg.DialTimeout,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
			PoolTimeout:  cfg.PoolTimeout,
			TLSConfig:    tlsCfg,
		}), nil
	case ModeSentinel:
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.Sentinel.MasterName,
			SentinelAddrs:    cfg.Addrs,
			SentinelUsername: cfg.Sentinel.Username,
			SentinelPassword: cfg.Sentinel.Password,
			Username:         cfg.Username,
			Password:         cfg.Password,
			DB:               cfg.DB,
			PoolSize:         cfg.PoolSize,
			MinIdleConns:     cfg.MinIdleConns,
			MaxRetries:       cfg.MaxRetries,
			DialTimeout:      cfg.DialTimeout,
			ReadTimeout:      cfg.ReadTimeout,
			WriteTimeout:     cfg.WriteTimeout,
			PoolTimeout:      cfg.PoolTimeout,
			TLSConfig:        tlsCfg,
		}), nil
	case ModeCluster:
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:         cfg.Addrs,
			Username:      cfg.Username,
			Password:      cfg.Password,
			MaxRedirects:  cfg.Cluster.MaxRedirects,
			RouteRandomly: cfg.Cluster.RouteRandomly,
			ReadOnly:      cfg.Cluster.ReadOnly,
			PoolSize:      cfg.PoolSize,
			MinIdleConns:  cfg.MinIdleConns,
			MaxRetries:    cfg.MaxRetries,
			DialTimeout:   cfg.DialTimeout,
			ReadTimeout:   cfg.ReadTimeout,
			WriteTimeout:  cfg.WriteTimeout,
			PoolTimeout:   cfg.PoolTimeout,
			TLSConfig:     tlsCfg,
		}), nil
	default:
		// Caught by validate(); kept as a defensive fallback so a future
		// mode added to validate() without a switch arm fails loudly.
		return nil, fmt.Errorf("redis: unknown mode %q", cfg.Mode)
	}
}

// validate enforces the per-mode field requirements documented on each Mode
// constant. Returned errors are designed to be human-readable on a service
// startup log line.
func validate(cfg Config) error {
	switch cfg.Mode {
	case ModeStandalone, ModeSentinel, ModeCluster:
		// valid
	default:
		return fmt.Errorf("redis: invalid mode %q (want standalone|sentinel|cluster)", cfg.Mode)
	}
	if len(cfg.Addrs) == 0 {
		return errors.New("redis: addrs is required")
	}
	for i, a := range cfg.Addrs {
		if a == "" {
			return fmt.Errorf("redis: addrs[%d] is empty", i)
		}
	}
	if cfg.Mode == ModeSentinel && cfg.Sentinel.MasterName == "" {
		return errors.New("redis: sentinel.masterName is required in sentinel mode")
	}
	if cfg.TLS.Enabled {
		// Both or neither — half-configured mTLS is a config bug we want
		// to surface at startup, not at first connection.
		hasCert := cfg.TLS.CertFile != ""
		hasKey := cfg.TLS.KeyFile != ""
		if hasCert != hasKey {
			return errors.New("redis: tls.certFile and tls.keyFile must be set together")
		}
	}
	return nil
}

// buildTLS materialises a [*tls.Config] from the TLS sub-block. Returns nil
// (no TLS) when [TLSConfig.Enabled] is false. addrs is consulted to derive
// the default SNI when [TLSConfig.ServerName] is blank.
func buildTLS(cfg TLSConfig, addrs []string) (*tls.Config, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	out := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // explicit opt-in via yaml
		MinVersion:         tls.VersionTLS12,
	}
	if cfg.ServerName != "" {
		out.ServerName = cfg.ServerName
	} else if len(addrs) > 0 {
		out.ServerName = hostOnly(addrs[0])
	}
	if cfg.CAFile != "" {
		pemBytes, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("caFile %s: no PEM certificates parsed", cfg.CAFile)
		}
		out.RootCAs = pool
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client keypair: %w", err)
		}
		out.Certificates = []tls.Certificate{pair}
	}
	return out, nil
}

// hostOnly strips the optional port from host:port; if no port is present
// the input is returned as-is. Used to derive a sensible default SNI from
// [Config.Addrs][0] when [TLSConfig.ServerName] is left blank.
func hostOnly(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
		if addr[i] == ']' {
			// IPv6 literal — no port to strip if ']' was last char.
			return addr
		}
	}
	return addr
}
