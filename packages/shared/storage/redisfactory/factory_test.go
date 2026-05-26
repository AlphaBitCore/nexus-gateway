package redisfactory

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// quietLogger discards every log line so test output stays clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNew_StandaloneAllFields drives a fully-populated standalone Config and
// asserts the factory returns a non-nil client with every field forwarded
// onto the underlying [*redis.Options] via the universal client interface.
func TestNew_StandaloneAllFields(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Mode:         ModeStandalone,
		Addrs:        []string{"127.0.0.1:6379"},
		Username:     "default",
		Password:     "secret",
		DB:           2,
		PoolSize:     12,
		MinIdleConns: 2,
		MaxRetries:   5,
		DialTimeout:  6 * time.Second,
		ReadTimeout:  4 * time.Second,
		WriteTimeout: 4 * time.Second,
		PoolTimeout:  5 * time.Second,
	}
	client, err := New(cfg, Env{}, quietLogger())
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("New: returned nil client")
	}
	t.Cleanup(func() { _ = client.Close() })
}

// TestNew_SentinelHappyPath builds a sentinel client when MasterName is set.
func TestNew_SentinelHappyPath(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Mode:  ModeSentinel,
		Addrs: []string{"127.0.0.1:26379", "127.0.0.1:26380"},
		Sentinel: SentinelConfig{
			MasterName: "mymaster",
			Username:   "sentinel-user",
			Password:   "sentinel-pass",
		},
		Username:    "data-user",
		Password:    "data-pass",
		DB:          1,
		DialTimeout: 5 * time.Second,
	}
	client, err := New(cfg, Env{}, quietLogger())
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("New: returned nil client")
	}
	t.Cleanup(func() { _ = client.Close() })
}

// TestNew_ClusterHappyPath builds a cluster client with all cluster knobs.
func TestNew_ClusterHappyPath(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Mode:  ModeCluster,
		Addrs: []string{"127.0.0.1:7000", "127.0.0.1:7001", "127.0.0.1:7002"},
		Cluster: ClusterConfig{
			MaxRedirects:  4,
			RouteRandomly: true,
			ReadOnly:      true,
		},
		PoolSize:    20,
		MaxRetries:  2,
		DialTimeout: 3 * time.Second,
	}
	client, err := New(cfg, Env{}, quietLogger())
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("New: returned nil client")
	}
	t.Cleanup(func() { _ = client.Close() })
}

// TestNew_DefaultModeIsStandalone verifies a blank Mode field is upgraded to
// standalone — the most common operator-friendly default.
func TestNew_DefaultModeIsStandalone(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Addrs: []string{"127.0.0.1:6379"},
	}
	client, err := New(cfg, Env{}, quietLogger())
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
}

// TestNew_EnvOverridesYaml proves env wins over yaml per the L3>L2 contract.
// yaml says addrs=["a:6379"] + db=0; env says REDIS_ADDRS="b:6379,c:6379" +
// REDIS_DB=7 → merged config has the env values.
func TestNew_EnvOverridesYaml(t *testing.T) {
	t.Parallel()
	yamlCfg := Config{
		Mode:     ModeStandalone,
		Addrs:    []string{"a:6379"},
		Username: "u1",
		Password: "p1",
		DB:       0,
	}
	addrs := []string{"b:6379", "c:6379"}
	user := "u2"
	pass := "p2"
	db := 7
	env := Env{
		Addrs:    addrs,
		Username: &user,
		Password: &pass,
		DB:       &db,
	}
	merged := mergeEnv(yamlCfg, env)
	if got, want := merged.Addrs, addrs; !equalStrings(got, want) {
		t.Errorf("addrs: got %v want %v", got, want)
	}
	if merged.Username != user {
		t.Errorf("username: got %q want %q", merged.Username, user)
	}
	if merged.Password != pass {
		t.Errorf("password: got %q want %q", merged.Password, pass)
	}
	if merged.DB != db {
		t.Errorf("db: got %d want %d", merged.DB, db)
	}
}

// TestMergeEnv_AllFields exercises every pointer field on Env so the merge
// fully covers the env-override path. Sentinel + cluster + TLS + pool +
// timeouts all flip from yaml defaults to env values.
func TestMergeEnv_AllFields(t *testing.T) {
	t.Parallel()
	mode := string(ModeSentinel)
	user := "user"
	pass := "pass"
	db := 9
	sentName := "master-x"
	sentUser := "su"
	sentPass := "sp"
	clusterMax := 8
	tt := true
	ff := false
	caFile := "/tmp/ca.pem"
	certFile := "/tmp/cert.pem"
	keyFile := "/tmp/key.pem"
	serverName := "redis.example.com"
	poolSize := 50
	minIdle := 4
	maxRetries := 7
	dial := 6 * time.Second
	read := 2 * time.Second
	write := 2 * time.Second
	poolT := 1 * time.Second

	env := Env{
		Mode:                  &mode,
		Addrs:                 []string{"x:1", "y:2"},
		Username:              &user,
		Password:              &pass,
		DB:                    &db,
		SentinelMasterName:    &sentName,
		SentinelUsername:      &sentUser,
		SentinelPassword:      &sentPass,
		ClusterMaxRedirects:   &clusterMax,
		ClusterRouteRandomly:  &tt,
		ClusterReadOnly:       &ff,
		TLSEnabled:            &tt,
		TLSInsecureSkipVerify: &ff,
		TLSCAFile:             &caFile,
		TLSCertFile:           &certFile,
		TLSKeyFile:            &keyFile,
		TLSServerName:         &serverName,
		PoolSize:              &poolSize,
		MinIdleConns:          &minIdle,
		MaxRetries:            &maxRetries,
		DialTimeout:           &dial,
		ReadTimeout:           &read,
		WriteTimeout:          &write,
		PoolTimeout:           &poolT,
	}
	merged := mergeEnv(Config{}, env)
	if merged.Mode != ModeSentinel {
		t.Errorf("mode: got %q", merged.Mode)
	}
	if !equalStrings(merged.Addrs, []string{"x:1", "y:2"}) {
		t.Errorf("addrs: %v", merged.Addrs)
	}
	if merged.Sentinel.MasterName != sentName ||
		merged.Sentinel.Username != sentUser ||
		merged.Sentinel.Password != sentPass {
		t.Errorf("sentinel sub-block: %+v", merged.Sentinel)
	}
	if merged.Cluster.MaxRedirects != clusterMax ||
		!merged.Cluster.RouteRandomly ||
		merged.Cluster.ReadOnly {
		t.Errorf("cluster sub-block: %+v", merged.Cluster)
	}
	if !merged.TLS.Enabled || merged.TLS.InsecureSkipVerify ||
		merged.TLS.CAFile != caFile ||
		merged.TLS.CertFile != certFile ||
		merged.TLS.KeyFile != keyFile ||
		merged.TLS.ServerName != serverName {
		t.Errorf("tls sub-block: %+v", merged.TLS)
	}
	if merged.PoolSize != poolSize ||
		merged.MinIdleConns != minIdle ||
		merged.MaxRetries != maxRetries {
		t.Errorf("pool fields: %+v", merged)
	}
	if merged.DialTimeout != dial ||
		merged.ReadTimeout != read ||
		merged.WriteTimeout != write ||
		merged.PoolTimeout != poolT {
		t.Errorf("timeout fields: %+v", merged)
	}
}

// TestValidate_Errors covers every named failure path on the validate()
// surface: unknown mode, blank addrs, blank element in addrs, sentinel
// missing master name, and mTLS with only one of cert/key.
func TestValidate_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		cfg   Config
		match string
	}{
		{
			name:  "unknown mode",
			cfg:   Config{Mode: "weird", Addrs: []string{"a:1"}},
			match: "invalid mode",
		},
		{
			name:  "no addrs",
			cfg:   Config{Mode: ModeStandalone},
			match: "addrs is required",
		},
		{
			name:  "blank addr",
			cfg:   Config{Mode: ModeStandalone, Addrs: []string{""}},
			match: "addrs[0] is empty",
		},
		{
			name:  "sentinel missing master",
			cfg:   Config{Mode: ModeSentinel, Addrs: []string{"a:1"}},
			match: "sentinel.masterName is required",
		},
		{
			name: "mTLS half-configured (cert only)",
			cfg: Config{
				Mode:  ModeStandalone,
				Addrs: []string{"a:1"},
				TLS:   TLSConfig{Enabled: true, CertFile: "c.pem"},
			},
			match: "certFile and tls.keyFile must be set together",
		},
		{
			name: "mTLS half-configured (key only)",
			cfg: Config{
				Mode:  ModeStandalone,
				Addrs: []string{"a:1"},
				TLS:   TLSConfig{Enabled: true, KeyFile: "k.pem"},
			},
			match: "certFile and tls.keyFile must be set together",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.cfg, Env{}, quietLogger())
			if err == nil {
				t.Fatalf("expected error matching %q, got nil", tc.match)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.match)
			}
		})
	}
}

// TestBuildTLS_Disabled returns a nil config when Enabled=false.
func TestBuildTLS_Disabled(t *testing.T) {
	t.Parallel()
	cfg, err := buildTLS(TLSConfig{Enabled: false}, []string{"a:1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil tls.Config, got %+v", cfg)
	}
}

// TestBuildTLS_HappyPath writes a real self-signed CA + client keypair to
// disk and verifies the loader returns a tls.Config with the right SNI,
// root pool, and one client certificate attached.
func TestBuildTLS_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	caPath := writeSelfSignedCA(t, dir)
	certPath, keyPath := writeClientKeypair(t, dir)

	out, err := buildTLS(TLSConfig{
		Enabled:    true,
		CAFile:     caPath,
		CertFile:   certPath,
		KeyFile:    keyPath,
		ServerName: "redis.test",
	}, []string{"ignored:1"})
	if err != nil {
		t.Fatalf("buildTLS: %v", err)
	}
	if out == nil {
		t.Fatal("buildTLS returned nil")
	}
	if out.ServerName != "redis.test" {
		t.Errorf("ServerName: got %q want %q", out.ServerName, "redis.test")
	}
	if out.RootCAs == nil {
		t.Error("RootCAs: expected populated pool")
	}
	if len(out.Certificates) != 1 {
		t.Errorf("Certificates: got %d, want 1", len(out.Certificates))
	}
}

// TestBuildTLS_ServerNameDerivedFromAddr proves that an unset ServerName
// falls back to the host portion of addrs[0].
func TestBuildTLS_ServerNameDerivedFromAddr(t *testing.T) {
	t.Parallel()
	out, err := buildTLS(TLSConfig{Enabled: true}, []string{"redis.example.com:6379"})
	if err != nil {
		t.Fatalf("buildTLS: %v", err)
	}
	if out.ServerName != "redis.example.com" {
		t.Errorf("ServerName: got %q", out.ServerName)
	}
}

// TestBuildTLS_NoAddrsKeepsServerNameBlank exercises the defensive branch
// where addrs is empty but TLS is enabled.
func TestBuildTLS_NoAddrsKeepsServerNameBlank(t *testing.T) {
	t.Parallel()
	out, err := buildTLS(TLSConfig{Enabled: true}, nil)
	if err != nil {
		t.Fatalf("buildTLS: %v", err)
	}
	if out.ServerName != "" {
		t.Errorf("ServerName: got %q, want empty", out.ServerName)
	}
}

// TestBuildTLS_CAFileMissing surfaces a clean error when the path is bad.
func TestBuildTLS_CAFileMissing(t *testing.T) {
	t.Parallel()
	_, err := buildTLS(TLSConfig{Enabled: true, CAFile: "/no/such/file.pem"}, nil)
	if err == nil || !strings.Contains(err.Error(), "read caFile") {
		t.Errorf("expected read-caFile error, got %v", err)
	}
}

// TestBuildTLS_CAFileNotPEM surfaces a clean error when the bytes contain
// no PEM blocks.
func TestBuildTLS_CAFileNotPEM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "not-pem.bin")
	if err := os.WriteFile(path, []byte("garbage bytes, not a PEM file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildTLS(TLSConfig{Enabled: true, CAFile: path}, nil)
	if err == nil || !strings.Contains(err.Error(), "no PEM certificates parsed") {
		t.Errorf("expected no-PEM error, got %v", err)
	}
}

// TestBuildTLS_BadKeypair returns an error when LoadX509KeyPair fails.
func TestBuildTLS_BadKeypair(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert := filepath.Join(dir, "c.pem")
	key := filepath.Join(dir, "k.pem")
	if err := os.WriteFile(cert, []byte("not a cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildTLS(TLSConfig{Enabled: true, CertFile: cert, KeyFile: key}, nil)
	if err == nil || !strings.Contains(err.Error(), "load client keypair") {
		t.Errorf("expected keypair-load error, got %v", err)
	}
}

// TestNew_TLSPropagationError surfaces a TLS error through New rather than
// silently constructing an unsafe client.
func TestNew_TLSPropagationError(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Mode:  ModeStandalone,
		Addrs: []string{"a:1"},
		TLS: TLSConfig{
			Enabled:  true,
			CertFile: "/tmp/no.crt",
			KeyFile:  "/tmp/no.key",
		},
	}
	_, err := New(cfg, Env{}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "redis tls") {
		t.Errorf("expected redis-tls wrapped error, got %v", err)
	}
}

// TestNew_TLSEnabledForAllModes proves the TLS path is wired through every
// mode's branch — standalone, sentinel, cluster. Uses a real CA file but
// no client cert (server-auth only).
func TestNew_TLSEnabledForAllModes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ca := writeSelfSignedCA(t, dir)
	for _, mode := range []Mode{ModeStandalone, ModeSentinel, ModeCluster} {

		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				Mode:     mode,
				Addrs:    []string{"127.0.0.1:6379"},
				Sentinel: SentinelConfig{MasterName: "mymaster"},
				TLS: TLSConfig{
					Enabled: true,
					CAFile:  ca,
				},
			}
			client, err := New(cfg, Env{}, quietLogger())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })
		})
	}
}

// TestHostOnly covers the SNI default heuristic on the three input shapes
// the function encounters in practice: host:port, bare host, and IPv6.
func TestHostOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"redis.example.com:6379", "redis.example.com"},
		{"redis.example.com", "redis.example.com"},
		{"127.0.0.1:6379", "127.0.0.1"},
		{"[::1]", "[::1]"},
	}
	for _, tc := range cases {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestLoadEnv_AllValues sets every documented REDIS_* variable in the test
// process environment, reloads it, and asserts every pointer field on Env
// is non-nil and matches.
func TestLoadEnv_AllValues(t *testing.T) {
	// Not parallel — uses t.Setenv on shared process state.
	t.Setenv("REDIS_MODE", "cluster")
	t.Setenv("REDIS_ADDRS", "a:1, b:2 ,c:3")
	t.Setenv("REDIS_USERNAME", "user")
	t.Setenv("REDIS_PASSWORD", "pass")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("REDIS_SENTINEL_MASTER_NAME", "mm")
	t.Setenv("REDIS_SENTINEL_USERNAME", "su")
	t.Setenv("REDIS_SENTINEL_PASSWORD", "sp")
	t.Setenv("REDIS_CLUSTER_MAX_REDIRECTS", "6")
	t.Setenv("REDIS_CLUSTER_ROUTE_RANDOMLY", "true")
	t.Setenv("REDIS_CLUSTER_READ_ONLY", "off")
	t.Setenv("REDIS_TLS_ENABLED", "yes")
	t.Setenv("REDIS_TLS_INSECURE", "0")
	t.Setenv("REDIS_TLS_CA_FILE", "/c/a")
	t.Setenv("REDIS_TLS_CERT_FILE", "/c/c")
	t.Setenv("REDIS_TLS_KEY_FILE", "/c/k")
	t.Setenv("REDIS_TLS_SERVER_NAME", "sn")
	t.Setenv("REDIS_POOL_SIZE", "33")
	t.Setenv("REDIS_MIN_IDLE_CONNS", "5")
	t.Setenv("REDIS_MAX_RETRIES", "9")
	t.Setenv("REDIS_DIAL_TIMEOUT", "7s")
	t.Setenv("REDIS_READ_TIMEOUT", "8s")
	t.Setenv("REDIS_WRITE_TIMEOUT", "9s")
	t.Setenv("REDIS_POOL_TIMEOUT", "10s")

	env := LoadEnv()
	if env.Mode == nil || *env.Mode != "cluster" {
		t.Errorf("Mode: %v", env.Mode)
	}
	if !equalStrings(env.Addrs, []string{"a:1", "b:2", "c:3"}) {
		t.Errorf("Addrs: %v", env.Addrs)
	}
	if env.Username == nil || *env.Username != "user" {
		t.Errorf("Username: %v", env.Username)
	}
	if env.DB == nil || *env.DB != 3 {
		t.Errorf("DB: %v", env.DB)
	}
	if env.SentinelMasterName == nil || *env.SentinelMasterName != "mm" {
		t.Errorf("SentinelMasterName: %v", env.SentinelMasterName)
	}
	if env.ClusterMaxRedirects == nil || *env.ClusterMaxRedirects != 6 {
		t.Errorf("ClusterMaxRedirects: %v", env.ClusterMaxRedirects)
	}
	if env.ClusterRouteRandomly == nil || !*env.ClusterRouteRandomly {
		t.Errorf("ClusterRouteRandomly: %v", env.ClusterRouteRandomly)
	}
	if env.ClusterReadOnly == nil || *env.ClusterReadOnly {
		t.Errorf("ClusterReadOnly: %v", env.ClusterReadOnly)
	}
	if env.TLSEnabled == nil || !*env.TLSEnabled {
		t.Errorf("TLSEnabled: %v", env.TLSEnabled)
	}
	if env.TLSInsecureSkipVerify == nil || *env.TLSInsecureSkipVerify {
		t.Errorf("TLSInsecureSkipVerify: %v", env.TLSInsecureSkipVerify)
	}
	if env.PoolSize == nil || *env.PoolSize != 33 {
		t.Errorf("PoolSize: %v", env.PoolSize)
	}
	if env.DialTimeout == nil || *env.DialTimeout != 7*time.Second {
		t.Errorf("DialTimeout: %v", env.DialTimeout)
	}
	if env.ReadTimeout == nil || *env.ReadTimeout != 8*time.Second {
		t.Errorf("ReadTimeout: %v", env.ReadTimeout)
	}
	if env.WriteTimeout == nil || *env.WriteTimeout != 9*time.Second {
		t.Errorf("WriteTimeout: %v", env.WriteTimeout)
	}
	if env.PoolTimeout == nil || *env.PoolTimeout != 10*time.Second {
		t.Errorf("PoolTimeout: %v", env.PoolTimeout)
	}
}

// TestLoadEnv_AllUnset returns a fully-nil Env when no REDIS_* var is set.
// We can't actually clear the parent process env here without
// race-affecting parallel tests, so we explicitly unset only the keys
// LoadEnv inspects via t.Setenv("", "") semantics is unavailable —
// instead we just call LoadEnv() inside a sub-test where the parent test
// process has no REDIS_* vars (CI does not set them).
func TestLoadEnv_MalformedIgnored(t *testing.T) {
	t.Setenv("REDIS_DB", "not-a-number")
	t.Setenv("REDIS_DIAL_TIMEOUT", "not-a-duration")
	t.Setenv("REDIS_TLS_ENABLED", "maybe")
	t.Setenv("REDIS_POOL_SIZE", "twelve")

	env := LoadEnv()
	if env.DB != nil {
		t.Errorf("DB: malformed value should be nil, got %v", *env.DB)
	}
	if env.DialTimeout != nil {
		t.Errorf("DialTimeout: malformed value should be nil, got %v", *env.DialTimeout)
	}
	if env.TLSEnabled != nil {
		t.Errorf("TLSEnabled: malformed value should be nil, got %v", *env.TLSEnabled)
	}
	if env.PoolSize != nil {
		t.Errorf("PoolSize: malformed value should be nil, got %v", *env.PoolSize)
	}
}

// TestLoadEnv_EmptyAddrsExplicit covers the "operator nulled out a yaml
// list" path — REDIS_ADDRS="" returns an empty (non-nil) slice so the
// merge replaces yaml addrs with nothing, surfacing a validate() error.
func TestLoadEnv_EmptyAddrsExplicit(t *testing.T) {
	t.Setenv("REDIS_ADDRS", " , , ")
	env := LoadEnv()
	if env.Addrs == nil {
		t.Fatal("Addrs: expected non-nil empty slice, got nil")
	}
	if len(env.Addrs) != 0 {
		t.Errorf("Addrs: expected empty slice, got %v", env.Addrs)
	}
}

// TestMergeEnv_NilPointersPreserveYaml proves the inverse of TestMergeEnv_AllFields
// — when env is the zero value, every yaml field survives the merge.
func TestMergeEnv_NilPointersPreserveYaml(t *testing.T) {
	t.Parallel()
	yamlCfg := Config{
		Mode:     ModeStandalone,
		Addrs:    []string{"a:1"},
		Username: "u",
		Password: "p",
		DB:       4,
		Sentinel: SentinelConfig{MasterName: "m"},
		Cluster:  ClusterConfig{MaxRedirects: 9},
		TLS: TLSConfig{
			Enabled:    true,
			CAFile:     "/ca",
			ServerName: "s",
		},
		PoolSize:     1,
		MinIdleConns: 2,
		MaxRetries:   3,
		DialTimeout:  1 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 3 * time.Second,
		PoolTimeout:  4 * time.Second,
	}
	merged := mergeEnv(yamlCfg, Env{})
	if merged.Mode != yamlCfg.Mode {
		t.Errorf("mode drift: %q vs %q", merged.Mode, yamlCfg.Mode)
	}
	if !equalStrings(merged.Addrs, yamlCfg.Addrs) {
		t.Errorf("addrs drift")
	}
	if merged.Username != yamlCfg.Username || merged.Password != yamlCfg.Password || merged.DB != yamlCfg.DB {
		t.Errorf("auth drift")
	}
	if merged.Sentinel != yamlCfg.Sentinel || merged.Cluster != yamlCfg.Cluster || merged.TLS != yamlCfg.TLS {
		t.Errorf("sub-block drift")
	}
}

// TestBoolPtrEnv_AllForms exercises every accepted truthy/falsy form plus
// the malformed-stays-nil path.
func TestBoolPtrEnv_AllForms(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "yes", "on"}
	falsy := []string{"0", "false", "FALSE", "no", "off"}
	for _, v := range truthy {
		t.Run("true/"+v, func(t *testing.T) {
			t.Setenv("REDIS_TEST_BOOL", v)
			got := boolPtrEnv("REDIS_TEST_BOOL")
			if got == nil || !*got {
				t.Errorf("expected true, got %v", got)
			}
		})
	}
	for _, v := range falsy {
		t.Run("false/"+v, func(t *testing.T) {
			t.Setenv("REDIS_TEST_BOOL", v)
			got := boolPtrEnv("REDIS_TEST_BOOL")
			if got == nil || *got {
				t.Errorf("expected false, got %v", got)
			}
		})
	}
	t.Run("malformed", func(t *testing.T) {
		t.Setenv("REDIS_TEST_BOOL", "maybe")
		if got := boolPtrEnv("REDIS_TEST_BOOL"); got != nil {
			t.Errorf("expected nil, got %v", *got)
		}
	})
	t.Run("unset", func(t *testing.T) {
		os.Unsetenv("REDIS_TEST_BOOL_UNSET")
		if got := boolPtrEnv("REDIS_TEST_BOOL_UNSET"); got != nil {
			t.Errorf("expected nil, got %v", *got)
		}
	})
}

// equalStrings is a tiny helper because slices.Equal lives in a tagged
// stdlib version we don't unconditionally pull in here; keeps the test file
// dependency-free.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// writeSelfSignedCA writes a fresh ECDSA-P256 self-signed CA certificate to
// dir and returns the on-disk path. Used by the buildTLS happy-path test.
func writeSelfSignedCA(t *testing.T, dir string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "redisfactory-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	return path
}

// writeClientKeypair writes a self-signed cert + matching private key to dir.
// Used by the buildTLS happy-path test to exercise the mTLS branch.
func writeClientKeypair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "redisfactory-test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	certPath = filepath.Join(dir, "client.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write client cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPath = filepath.Join(dir, "client.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}
	return certPath, keyPath
}
