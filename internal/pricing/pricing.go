// Package pricing turns token usage into dollars.
package pricing

import (
	"github.com/kuini-1/llm-gateway/internal/config"
	"github.com/kuini-1/llm-gateway/internal/provider"
)

type Table map[string]config.Price

const perToken = 1e-6

// Cost of a finished call. ok is false when the model has no price.
func (t Table) Cost(model string, u provider.Usage) (usd float64, ok bool) {
	p, ok := t[model]
	if !ok {
		return 0, false
	}
	return perToken * (float64(u.InputTokens)*p.Input + float64(u.OutputTokens)*p.Output +
		float64(u.CacheReadTokens)*p.CacheRead + float64(u.CacheWriteTokens)*p.CacheWrite), true
}

// MaxCost is the most a chat call can cost: every input token at the full
// input price plus max_tokens of output. Spend caps reserve this up front.
func (t Table) MaxCost(model string, inputTokens, maxTokens int64) float64 {
	p, ok := t[model]
	if !ok {
		return 0
	}
	cacheWrite := max(p.Input, p.CacheWrite) // a cache write can cost more than plain input
	return perToken * (float64(inputTokens)*cacheWrite + float64(maxTokens)*p.Output)
}

// EmbedCost prices embedding input tokens.
func (t Table) EmbedCost(model string, tokens int64) float64 {
	return perToken * float64(tokens) * t[model].Input
}

// EstimateTokens approximates the token count of text (about 4 characters
// per token). It is only used to size reservations, never for billing.
func EstimateTokens(s string) int64 { return int64(len(s)+3) / 4 }
