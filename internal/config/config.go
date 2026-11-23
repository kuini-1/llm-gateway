// Package config loads the gateway's YAML configuration: providers, the
// routes clients can call, model prices, cache and limit settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen         string              `yaml:"listen"`
	PostgresURL    string              `yaml:"postgres_url"`
	RedisURL       string              `yaml:"redis_url"`
	Providers      map[string]Provider `yaml:"providers"`
	Routes         map[string]Route    `yaml:"routes"`
	Prices         map[string]Price    `yaml:"prices"` // key: model name
	EmbeddingCache EmbeddingCache      `yaml:"embedding_cache"`
	Breaker        Breaker             `yaml:"circuit_breaker"`
	Defaults       Defaults            `yaml:"defaults"`
}

// Provider is one upstream API.
type Provider struct {
	Kind    string        `yaml:"kind"`     // "anthropic" or "openai" (OpenAI-compatible: OpenAI, DeepSeek, local servers)
	BaseURL string        `yaml:"base_url"` // empty = the provider's default
	APIKey  string        `yaml:"api_key"`  // usually "${ENV_VAR}"
	Timeout time.Duration `yaml:"timeout"`  // per attempt
	// ServerSideFallback lets Anthropic retry a refused request on a fallback
	// model inside the same call (beta). Gateway-level fallback still applies.
	ServerSideFallback bool `yaml:"server_side_fallback"`
	// PromptCaching adds automatic prompt caching to Anthropic requests.
	PromptCaching bool `yaml:"prompt_caching"`
}

// Target is one place a route can send a request.
type Target struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Route is what clients put in the "model" field. Targets are tried in
// order; the next one is used when the previous fails with a retryable error.
type Route struct {
	Kind    string   `yaml:"kind"` // "chat" or "embedding"
	Targets []Target `yaml:"targets"`
	// FallbackOnRefusal also moves on when a model declines the request.
	FallbackOnRefusal bool `yaml:"fallback_on_refusal"`
}

// Price is USD per million tokens.
type Price struct {
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

type EmbeddingCache struct {
	Enabled bool          `yaml:"enabled"`
	TTL     time.Duration `yaml:"ttl"`
}

type Breaker struct {
	FailureThreshold int           `yaml:"failure_threshold"` // consecutive failures that open the circuit
	OpenFor          time.Duration `yaml:"open_for"`          // how long to skip the target before probing
}

type Defaults struct {
	MaxTokens     int  `yaml:"max_tokens"`
	AllowUnpriced bool `yaml:"allow_unpriced"` // allow models with no price (cost recorded as 0, caps can't apply)
	RPM           int  `yaml:"rpm"`            // per API key, when the key sets none
}

var envRef = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// Load reads the file and expands ${ENV} references.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := envRef.ReplaceAllStringFunc(string(raw), func(m string) string {
		return os.Getenv(envRef.FindStringSubmatch(m)[1])
	})
	c := &Config{}
	if err := yaml.Unmarshal([]byte(expanded), c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults()
	return c, c.Validate()
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Breaker.FailureThreshold == 0 {
		c.Breaker.FailureThreshold = 5
	}
	if c.Breaker.OpenFor == 0 {
		c.Breaker.OpenFor = 30 * time.Second
	}
	if c.EmbeddingCache.TTL == 0 {
		c.EmbeddingCache.TTL = 30 * 24 * time.Hour
	}
	if c.Defaults.MaxTokens == 0 {
		c.Defaults.MaxTokens = 16000
	}
	if c.Defaults.RPM == 0 {
		c.Defaults.RPM = 600
	}
	for name, p := range c.Providers {
		if p.Timeout == 0 {
			p.Timeout = 120 * time.Second
		}
		c.Providers[name] = p
	}
}

func (c *Config) Validate() error {
	var errs []error
	if len(c.Routes) == 0 {
		errs = append(errs, errors.New("no routes configured"))
	}
	for name, p := range c.Providers {
		if p.Kind != "anthropic" && p.Kind != "openai" {
			errs = append(errs, fmt.Errorf("provider %q: kind must be anthropic or openai", name))
		}
	}
	for name, r := range c.Routes {
		if r.Kind != "chat" && r.Kind != "embedding" {
			errs = append(errs, fmt.Errorf("route %q: kind must be chat or embedding", name))
		}
		if len(r.Targets) == 0 {
			errs = append(errs, fmt.Errorf("route %q has no targets", name))
		}
		for _, t := range r.Targets {
			p, ok := c.Providers[t.Provider]
			if !ok {
				errs = append(errs, fmt.Errorf("route %q: unknown provider %q", name, t.Provider))
				continue
			}
			if r.Kind == "embedding" && p.Kind == "anthropic" {
				errs = append(errs, fmt.Errorf("route %q: provider %q has no embeddings API", name, t.Provider))
			}
			if _, priced := c.Prices[t.Model]; !priced && !c.Defaults.AllowUnpriced {
				errs = append(errs, fmt.Errorf("route %q: model %q has no price (add it under prices, or set defaults.allow_unpriced)", name, t.Model))
			}
		}
	}
	return errors.Join(errs...)
}
