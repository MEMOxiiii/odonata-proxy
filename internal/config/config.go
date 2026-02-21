// Package config loads and validates the proxy configuration from a YAML file.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure for the proxy.
type Config struct {
	Proxy     ProxyConfig     `yaml:"proxy"`
	Backends  []BackendConfig `yaml:"backends"`
	RateLimit RateLimitConfig `yaml:"rate_limit"`
	Logging   LoggingConfig   `yaml:"logging"`
}

// ProxyConfig holds network-level settings for the proxy listener.
type ProxyConfig struct {
	// Listen is the address:port the proxy binds to. Example: "0.0.0.0:19132"
	Listen string `yaml:"listen"`

	// MOTD is the server name shown in the Bedrock multiplayer list.
	MOTD string `yaml:"motd"`

	// MaxPlayers is the maximum number of concurrent connections accepted.
	MaxPlayers int `yaml:"max_players"`

	// AuthDisabled disables Xbox Live authentication at the proxy level.
	// Enable when backends run auth-disabled Dragonfly instances on localhost.
	AuthDisabled bool `yaml:"auth_disabled"`

	// TexturePacksRequired forces clients to accept packs before login.
	TexturePacksRequired bool `yaml:"texture_packs_required"`

	// ResourcePacks is a list of paths to .mcpack files or directories.
	ResourcePacks []string `yaml:"resource_packs"`
}

// BackendConfig describes a single backend Dragonfly server.
type BackendConfig struct {
	// Name is a human-readable identifier used in logs and API calls.
	Name string `yaml:"name"`

	// Address is "host:port" of the backend server.
	Address string `yaml:"address"`

	// Default marks this backend as the initial server for new connections.
	Default bool `yaml:"default"`

	// WarmPoolSize is the number of pre-dialed connections to maintain.
	// Set to 0 to disable pre-warming.
	WarmPoolSize int `yaml:"warm_pool_size"`
}

// RateLimitConfig controls per-IP token-bucket rate limiting.
type RateLimitConfig struct {
	Enabled          bool          `yaml:"enabled"`
	PacketsPerSecond float64       `yaml:"packets_per_second"`
	BurstSize        int           `yaml:"burst_size"`
	CleanupInterval  time.Duration `yaml:"cleanup_interval"`
}

// LoggingConfig controls structured log output.
type LoggingConfig struct {
	// Level: debug | info | warn | error
	Level string `yaml:"level"`
	// Format: console | json
	Format     string `yaml:"format"`
	OutputFile string `yaml:"output_file"`
}

// Load reads, decodes, and validates the YAML config at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config file %q: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config file %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

// DefaultBackend returns the BackendConfig marked as default.
// Returns the first backend if none is explicitly marked.
func (c *Config) DefaultBackend() BackendConfig {
	for _, b := range c.Backends {
		if b.Default {
			return b
		}
	}
	return c.Backends[0]
}

// BackendByName looks up a backend by its Name field.
func (c *Config) BackendByName(name string) (BackendConfig, bool) {
	for _, b := range c.Backends {
		if b.Name == name {
			return b, true
		}
	}
	return BackendConfig{}, false
}

func (c *Config) validate() error {
	if c.Proxy.Listen == "" {
		return fmt.Errorf("proxy.listen must not be empty")
	}
	if len(c.Backends) == 0 {
		return fmt.Errorf("at least one backend must be defined")
	}
	for i, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("backends[%d].name must not be empty", i)
		}
		if b.Address == "" {
			return fmt.Errorf("backends[%d].address must not be empty", i)
		}
	}
	if c.RateLimit.Enabled {
		if c.RateLimit.PacketsPerSecond <= 0 {
			return fmt.Errorf("rate_limit.packets_per_second must be > 0")
		}
		if c.RateLimit.BurstSize <= 0 {
			return fmt.Errorf("rate_limit.burst_size must be > 0")
		}
	}
	return nil
}

func applyDefaults(c *Config) {
	if c.RateLimit.CleanupInterval == 0 {
		c.RateLimit.CleanupInterval = 30 * time.Second
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "console"
	}
	if c.Proxy.MaxPlayers == 0 {
		c.Proxy.MaxPlayers = 1000
	}
}
