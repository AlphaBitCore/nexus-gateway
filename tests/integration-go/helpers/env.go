// Package helpers wires Phase 2 Go integration tests to the same
// configuration the shell smoke scripts and Python e2e tests read from
// tests/.env.<target>. Keeping the env contract identical across all
// three stacks (bash via loadenv.sh, Python via loadenv.py, Go here)
// means an operator only edits one file per target to repoint a test
// run. Target selection mirrors the bash/Python loaders:
//   1. NEXUS_TEST_TARGET env var (preferred).
//   2. Defaults to "local" — matches the loadenv.sh TTY-default.
package helpers

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Env carries the resolved test configuration.
type Env struct {
	HubURL          string
	CPURL           string
	AIGwURL         string
	ProxyURL        string
	UIURL           string
	AdminEmail      string
	AdminPassword   string
	OAuthClientID   string
	OAuthRedirect   string
	HubServiceToken string
	TestVK          string
	JudgeModel      string
	PGHost          string
	PGPort          string
	PGUser          string
	PGPassword      string
	PGDB            string
}

var (
	envOnce sync.Once
	envVal  *Env
	envErr  error
)

// LoadEnv reads tests/.env.<target> (falling back to .env.<target>.example)
// once per process and returns the same Env on every call. Real environment
// variables override file values so CI overrides Just Work.
//
// Target comes from NEXUS_TEST_TARGET (default "local"). Same selection
// rule as tests/lib/loadenv.{sh,py}.
func LoadEnv() (*Env, error) {
	envOnce.Do(func() {
		envVal, envErr = loadEnvOnce()
	})
	return envVal, envErr
}

func loadEnvOnce() (*Env, error) {
	root, err := repoTestsRoot()
	if err != nil {
		return nil, err
	}
	target := os.Getenv("NEXUS_TEST_TARGET")
	if target == "" {
		target = "local"
	}
	values := map[string]string{}
	// Example first so .env.<target> can override.
	_ = readDotEnv(filepath.Join(root, ".env."+target+".example"), values)
	_ = readDotEnv(filepath.Join(root, ".env."+target), values)

	get := func(key, dflt string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v, ok := values[key]; ok && v != "" {
			return v
		}
		return dflt
	}

	return &Env{
		HubURL:          get("NEXUS_HUB_URL", "http://localhost:3060"),
		CPURL:           get("NEXUS_CP_URL", "http://localhost:3001"),
		AIGwURL:         get("NEXUS_AI_GW_URL", "http://localhost:3050"),
		ProxyURL:        get("NEXUS_PROXY_URL", "http://localhost:3040"),
		UIURL:           get("NEXUS_UI_URL", "http://localhost:3000"),
		AdminEmail:      get("NEXUS_ADMIN_EMAIL", "admin@nexus.ai"),
		AdminPassword:   get("NEXUS_ADMIN_PASSWORD", "admin123"),
		OAuthClientID:   get("NEXUS_OAUTH_CLIENT_ID", "cp-ui"),
		OAuthRedirect:   get("NEXUS_OAUTH_REDIRECT_URI", "http://localhost:3000/auth/callback"),
		HubServiceToken: get("NEXUS_HUB_SERVICE_TOKEN", "dev-service-token"),
		TestVK:          get("NEXUS_TEST_VK", ""),
		JudgeModel:      get("NEXUS_JUDGE_MODEL", "moonshot-v1-128k"),
		PGHost:          get("NEXUS_PG_HOST", "localhost"),
		PGPort:          get("NEXUS_PG_PORT", "55532"),
		PGUser:          get("NEXUS_PG_USER", "postgres"),
		PGPassword:      get("NEXUS_PG_PASSWORD", "postgres"),
		PGDB:            get("NEXUS_PG_DB", "nexus_gateway"),
	}, nil
}

func repoTestsRoot() (string, error) {
	// Walk up from the test binary's PWD until we find the tests/ marker.
	// Tests are normally invoked from tests/integration-go/ so two levels
	// up is the repo root; we look for the canonical .env.local.example
	// (renamed from .env.test.example 2026-05-16) to confirm we're in the
	// right place rather than counting parents.
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		marker := filepath.Join(dir, "tests", ".env.local.example")
		if _, err := os.Stat(marker); err == nil {
			return filepath.Join(dir, "tests"), nil
		}
		// Also accept being run from inside tests/ itself.
		if _, err := os.Stat(filepath.Join(dir, ".env.local.example")); err == nil {
			return dir, nil
		}
	}
	return "", os.ErrNotExist
}

func readDotEnv(path string, into map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"`)
		into[key] = value
	}
	return scanner.Err()
}

// PGDSN returns a libpq-style DSN suitable for pgx.Connect.
func (e *Env) PGDSN() string {
	return "host=" + e.PGHost +
		" port=" + e.PGPort +
		" user=" + e.PGUser +
		" password=" + e.PGPassword +
		" dbname=" + e.PGDB
}
