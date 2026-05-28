package core

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Env is one named target. It holds only non-secret references; secrets
// (tokens, admin key, VK secret) live in the OS keychain (see SecretStore).
type Env struct {
	Name             string `toml:"name"`
	CPBaseURL        string `toml:"cp_base_url"`
	AIGatewayBaseURL string `toml:"ai_gateway_base_url"`
	OAuthClientID    string `toml:"oauth_client_id"`
	OAuthRedirectURI string `toml:"oauth_redirect_uri"` // headless/registered redirect; browser flow uses a loopback URI
	IsProd           bool   `toml:"is_prod"`
	LastModel        string `toml:"last_model"`
	LastVKID         string `toml:"last_vk_id"`
	LastVKName       string `toml:"last_vk_name"`
}

// Config is the on-disk profile set at ~/.config/nexus/config.toml.
type Config struct {
	DefaultEnv string         `toml:"default_env"`
	Envs       map[string]Env `toml:"envs"`

	path string // source path, not serialized
}

// builtinLocalEnv is the out-of-the-box local target so a fresh clone can
// `nexus --env local login` without hand-writing a config first.
func builtinLocalEnv() Env {
	return Env{
		Name:             "local",
		CPBaseURL:        "http://localhost:3001",
		AIGatewayBaseURL: "http://localhost:3050",
		OAuthClientID:    "cp-ui",
		OAuthRedirectURI: "http://localhost:3000/auth/callback",
		IsProd:           false,
	}
}

// DefaultConfigPath returns ~/.config/nexus/config.toml (OS-appropriate).
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "nexus", "config.toml"), nil
}

// newDefaultConfig is the seed used when no config file exists yet.
func newDefaultConfig(path string) *Config {
	return &Config{
		DefaultEnv: "local",
		Envs:       map[string]Env{"local": builtinLocalEnv()},
		path:       path,
	}
}

// Load reads the config at path. A missing file is not an error: it returns the
// built-in defaults (local target, default_env=local) so first run works.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newDefaultConfig(path), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := toml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if c.Envs == nil {
		c.Envs = map[string]Env{}
	}
	c.path = path
	return &c, nil
}

// Save writes the config back to its source path, creating the directory with
// 0700 and the file with 0600. It never writes secrets — Env holds none.
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no path")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.OpenFile(c.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open config for write: %w", err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
}

// Resolve picks the active environment using the precedence
// flagEnv > sessionEnv > default_env (FR-5). It returns a clear error when no
// name resolves or the named env is not defined.
func (c *Config) Resolve(flagEnv, sessionEnv string) (Env, error) {
	name := firstNonEmpty(flagEnv, sessionEnv, c.DefaultEnv)
	if name == "" {
		return Env{}, fmt.Errorf("no environment selected: pass --env, switch in-session, or set default_env")
	}
	env, ok := c.Envs[name]
	if !ok {
		return Env{}, fmt.Errorf("unknown environment %q (defined: %v)", name, c.envNames())
	}
	if env.Name == "" {
		env.Name = name
	}
	return env, nil
}

// SetEnv inserts or replaces a named environment.
func (c *Config) SetEnv(env Env) {
	if c.Envs == nil {
		c.Envs = map[string]Env{}
	}
	c.Envs[env.Name] = env
}

// RemoveEnv deletes a named environment. Removing the current default clears
// default_env (the operator picks a new one or it falls back to built-in local).
func (c *Config) RemoveEnv(name string) error {
	if _, ok := c.Envs[name]; !ok {
		return fmt.Errorf("unknown environment %q", name)
	}
	delete(c.Envs, name)
	if c.DefaultEnv == name {
		c.DefaultEnv = ""
	}
	return nil
}

// SetDefault points default_env at name, erroring if name is undefined.
func (c *Config) SetDefault(name string) error {
	if _, ok := c.Envs[name]; !ok {
		return fmt.Errorf("unknown environment %q", name)
	}
	c.DefaultEnv = name
	return nil
}

func (c *Config) envNames() []string {
	names := make([]string, 0, len(c.Envs))
	for n := range c.Envs {
		names = append(names, n)
	}
	return names
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
