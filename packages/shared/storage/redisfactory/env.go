package redisfactory

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Env collects every REDIS_* environment variable the factory understands.
// Each field is a *T pointer so an unset variable can be distinguished from
// an explicit zero value at merge time (e.g. REDIS_DB=0 differs from "the
// operator left this knob alone").
//
// Use [LoadEnv] to populate from os.Environ() and pass the result to [New]
// alongside the yaml-derived [Config]. Callers should not construct Env
// values by hand outside of tests.
type Env struct {
	Mode     *string
	Addrs    []string // nil = unset; empty slice = explicit empty list
	Username *string
	Password *string
	DB       *int

	SentinelMasterName *string
	SentinelUsername   *string
	SentinelPassword   *string

	ClusterMaxRedirects  *int
	ClusterRouteRandomly *bool
	ClusterReadOnly      *bool

	TLSEnabled            *bool
	TLSInsecureSkipVerify *bool
	TLSCAFile             *string
	TLSCertFile           *string
	TLSKeyFile            *string
	TLSServerName         *string

	PoolSize     *int
	MinIdleConns *int
	MaxRetries   *int

	DialTimeout  *time.Duration
	ReadTimeout  *time.Duration
	WriteTimeout *time.Duration
	PoolTimeout  *time.Duration
}

// LoadEnv reads every REDIS_* variable from the process environment and
// returns the populated [Env]. Unset variables stay nil; malformed values
// (e.g. REDIS_DB="not-a-number") are silently ignored so a typo in one
// knob does not nuke the whole startup. Operators see the surviving yaml
// default in that case.
func LoadEnv() Env {
	return Env{
		Mode:     strPtrEnv("REDIS_MODE"),
		Addrs:    splitEnv("REDIS_ADDRS"),
		Username: strPtrEnv("REDIS_USERNAME"),
		Password: strPtrEnv("REDIS_PASSWORD"),
		DB:       intPtrEnv("REDIS_DB"),

		SentinelMasterName: strPtrEnv("REDIS_SENTINEL_MASTER_NAME"),
		SentinelUsername:   strPtrEnv("REDIS_SENTINEL_USERNAME"),
		SentinelPassword:   strPtrEnv("REDIS_SENTINEL_PASSWORD"),

		ClusterMaxRedirects:  intPtrEnv("REDIS_CLUSTER_MAX_REDIRECTS"),
		ClusterRouteRandomly: boolPtrEnv("REDIS_CLUSTER_ROUTE_RANDOMLY"),
		ClusterReadOnly:      boolPtrEnv("REDIS_CLUSTER_READ_ONLY"),

		TLSEnabled:            boolPtrEnv("REDIS_TLS_ENABLED"),
		TLSInsecureSkipVerify: boolPtrEnv("REDIS_TLS_INSECURE"),
		TLSCAFile:             strPtrEnv("REDIS_TLS_CA_FILE"),
		TLSCertFile:           strPtrEnv("REDIS_TLS_CERT_FILE"),
		TLSKeyFile:            strPtrEnv("REDIS_TLS_KEY_FILE"),
		TLSServerName:         strPtrEnv("REDIS_TLS_SERVER_NAME"),

		PoolSize:     intPtrEnv("REDIS_POOL_SIZE"),
		MinIdleConns: intPtrEnv("REDIS_MIN_IDLE_CONNS"),
		MaxRetries:   intPtrEnv("REDIS_MAX_RETRIES"),

		DialTimeout:  durPtrEnv("REDIS_DIAL_TIMEOUT"),
		ReadTimeout:  durPtrEnv("REDIS_READ_TIMEOUT"),
		WriteTimeout: durPtrEnv("REDIS_WRITE_TIMEOUT"),
		PoolTimeout:  durPtrEnv("REDIS_POOL_TIMEOUT"),
	}
}

// mergeEnv overlays env values onto yamlCfg, returning the merged result.
// Pointer fields on env that are nil leave yamlCfg untouched. A non-nil
// pointer wins regardless of yaml content — the contract is "env > yaml"
// per the configuration architecture's L3 > L2 precedence.
func mergeEnv(yamlCfg Config, env Env) Config {
	out := yamlCfg
	if env.Mode != nil {
		out.Mode = Mode(*env.Mode)
	}
	if env.Addrs != nil {
		out.Addrs = env.Addrs
	}
	if env.Username != nil {
		out.Username = *env.Username
	}
	if env.Password != nil {
		out.Password = *env.Password
	}
	if env.DB != nil {
		out.DB = *env.DB
	}
	if env.SentinelMasterName != nil {
		out.Sentinel.MasterName = *env.SentinelMasterName
	}
	if env.SentinelUsername != nil {
		out.Sentinel.Username = *env.SentinelUsername
	}
	if env.SentinelPassword != nil {
		out.Sentinel.Password = *env.SentinelPassword
	}
	if env.ClusterMaxRedirects != nil {
		out.Cluster.MaxRedirects = *env.ClusterMaxRedirects
	}
	if env.ClusterRouteRandomly != nil {
		out.Cluster.RouteRandomly = *env.ClusterRouteRandomly
	}
	if env.ClusterReadOnly != nil {
		out.Cluster.ReadOnly = *env.ClusterReadOnly
	}
	if env.TLSEnabled != nil {
		out.TLS.Enabled = *env.TLSEnabled
	}
	if env.TLSInsecureSkipVerify != nil {
		out.TLS.InsecureSkipVerify = *env.TLSInsecureSkipVerify
	}
	if env.TLSCAFile != nil {
		out.TLS.CAFile = *env.TLSCAFile
	}
	if env.TLSCertFile != nil {
		out.TLS.CertFile = *env.TLSCertFile
	}
	if env.TLSKeyFile != nil {
		out.TLS.KeyFile = *env.TLSKeyFile
	}
	if env.TLSServerName != nil {
		out.TLS.ServerName = *env.TLSServerName
	}
	if env.PoolSize != nil {
		out.PoolSize = *env.PoolSize
	}
	if env.MinIdleConns != nil {
		out.MinIdleConns = *env.MinIdleConns
	}
	if env.MaxRetries != nil {
		out.MaxRetries = *env.MaxRetries
	}
	if env.DialTimeout != nil {
		out.DialTimeout = *env.DialTimeout
	}
	if env.ReadTimeout != nil {
		out.ReadTimeout = *env.ReadTimeout
	}
	if env.WriteTimeout != nil {
		out.WriteTimeout = *env.WriteTimeout
	}
	if env.PoolTimeout != nil {
		out.PoolTimeout = *env.PoolTimeout
	}
	return out
}

// strPtrEnv returns a pointer to the named env value, or nil when unset.
// Distinguishing "unset" from "set to empty" lets callers explicitly null
// out a yaml-supplied password via REDIS_PASSWORD="" if they ever need to.
func strPtrEnv(key string) *string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	return &v
}

// intPtrEnv returns a parsed int from the env, or nil on unset / malformed.
func intPtrEnv(key string) *int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return nil
	}
	return &n
}

// boolPtrEnv parses 1/true/yes (case-insensitive) as true and 0/false/no as
// false. Other values produce nil so a typo doesn't silently flip a flag.
func boolPtrEnv(key string) *bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		t := true
		return &t
	case "0", "false", "no", "off":
		f := false
		return &f
	}
	return nil
}

// durPtrEnv parses Go time.ParseDuration values (e.g. "5s", "200ms").
func durPtrEnv(key string) *time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return nil
	}
	return &d
}

// splitEnv splits a comma-separated env value into a trimmed, empty-stripped
// slice. Returns nil when the variable is unset; returns an empty slice when
// the variable is set but contains no non-blank entries (so a deliberate
// REDIS_ADDRS="" can null out a yaml addrs list).
func splitEnv(key string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
