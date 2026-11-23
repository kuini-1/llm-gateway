package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped configs must load and validate.
func TestShippedConfigsLoad(t *testing.T) {
	t.Setenv("GATEWAY_POSTGRES_URL", "postgres://x")
	t.Setenv("GATEWAY_REDIS_URL", "redis://x")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	for _, p := range []string{"../../gateway.example.yaml", "../../bench/gateway.bench.yaml", "../../deploy/gateway.compose.yaml"} {
		c, err := Load(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if c.PostgresURL != "postgres://x" {
			t.Errorf("%s: env not expanded", p)
		}
	}
	c, _ := Load("../../gateway.example.yaml")
	if c.Providers["anthropic"].APIKey != "sk-test" {
		t.Error("api key not expanded from the environment")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"unknown provider": `
routes:
  r: {kind: chat, targets: [{provider: nope, model: m}]}
prices: {m: {input: 1}}`,
		"embedding fallback to another model": `
providers: {a: {kind: openai}, b: {kind: openai}}
routes:
  e: {kind: embedding, targets: [{provider: a, model: m1}, {provider: b, model: m2}]}
prices: {m1: {input: 1}, m2: {input: 1}}`,
		"embeddings from anthropic": `
providers: {a: {kind: anthropic}}
routes:
  e: {kind: embedding, targets: [{provider: a, model: m}]}
prices: {m: {input: 1}}`,
		"unpriced model": `
providers: {a: {kind: openai}}
routes:
  r: {kind: chat, targets: [{provider: a, model: m}]}`,
	}
	dir := t.TempDir()
	for name, body := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".yaml")
		os.WriteFile(p, []byte(body), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}
