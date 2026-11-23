// Package router resolves the "model" a client asked for to a list of
// upstream targets and tries them in order, skipping targets whose circuit
// breaker is open and moving on only when the error is one another
// provider could avoid.
package router

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kuini-1/llm-gateway/internal/config"
	"github.com/kuini-1/llm-gateway/internal/provider"
)

type Router struct {
	routes   map[string]config.Route
	chat     map[string]provider.Chatter
	embed    map[string]provider.Embedder
	breakers map[string]*Breaker // by "provider/model"
	cfg      config.Breaker
}

func New(cfg *config.Config) *Router {
	r := &Router{
		routes: cfg.Routes, chat: map[string]provider.Chatter{}, embed: map[string]provider.Embedder{},
		breakers: map[string]*Breaker{}, cfg: cfg.Breaker,
	}
	for name, p := range cfg.Providers {
		switch p.Kind {
		case "anthropic":
			r.chat[name] = provider.NewAnthropic(name, p.BaseURL, p.APIKey, p.Timeout, p.ServerSideFallback, p.PromptCaching)
		case "openai":
			o := provider.NewOpenAICompat(name, p.BaseURL, p.APIKey, p.Timeout)
			r.chat[name], r.embed[name] = o, o
		}
	}
	for _, route := range cfg.Routes {
		for _, t := range route.Targets {
			r.breaker(t)
		}
	}
	return r
}

// SetProvider replaces a provider (tests use it to inject fakes).
func (r *Router) SetProvider(name string, c provider.Chatter, e provider.Embedder) {
	if c != nil {
		r.chat[name] = c
	}
	if e != nil {
		r.embed[name] = e
	}
}

func (r *Router) breaker(t config.Target) *Breaker {
	key := t.Provider + "/" + t.Model
	b, ok := r.breakers[key]
	if !ok {
		b = NewBreaker(r.cfg.FailureThreshold, r.cfg.OpenFor)
		r.breakers[key] = b
	}
	return b
}

var ErrUnknownModel = errors.New("unknown model")

// Resolve returns the route for a model name. Besides configured route
// names, "provider/model" addresses one target directly (no fallback).
func (r *Router) Resolve(model, kind string) (config.Route, error) {
	if rt, ok := r.routes[model]; ok {
		if rt.Kind != kind {
			return rt, fmt.Errorf("%w: %q is a %s route, not %s", ErrUnknownModel, model, rt.Kind, kind)
		}
		return rt, nil
	}
	if prov, m, ok := strings.Cut(model, "/"); ok {
		if _, known := r.chat[prov]; known {
			if _, isEmbed := r.embed[prov]; kind == "embedding" && !isEmbed {
				return config.Route{}, fmt.Errorf("%w: provider %q has no embeddings API", ErrUnknownModel, prov)
			}
			return config.Route{Kind: kind, Targets: []config.Target{{Provider: prov, Model: m}}}, nil
		}
	}
	return config.Route{}, fmt.Errorf("%w %q (see GET /v1/models)", ErrUnknownModel, model)
}

// Attempt records one try at one target.
type Attempt struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"` // circuit open
}

type ChatResult struct {
	Resp     *provider.ChatResponse
	Target   config.Target
	Attempts []Attempt
	TTFB     time.Duration // to the first streamed token
}

// FallbackFrom is the first target tried, when it isn't the one that answered.
func (c *ChatResult) FallbackFrom() string {
	for _, a := range c.Attempts {
		if !a.Skipped {
			if a.Provider != c.Target.Provider || a.Model != c.Target.Model {
				return a.Provider + "/" + a.Model
			}
			return ""
		}
	}
	return ""
}

// AllFailed is returned when no target produced an answer.
type AllFailed struct {
	Attempts []Attempt
	Last     *provider.Error
}

func (e *AllFailed) Error() string {
	var parts []string
	for _, a := range e.Attempts {
		if a.Skipped {
			parts = append(parts, a.Provider+"/"+a.Model+": skipped (circuit open)")
		} else {
			parts = append(parts, a.Provider+"/"+a.Model+": "+a.Error)
		}
	}
	return "all targets failed: " + strings.Join(parts, "; ")
}

// order puts allowed targets first, keeping the configured order; when every
// breaker is open it still tries the first target rather than fail without
// trying.
func (r *Router) order(rt config.Route) (try []config.Target, skipped []config.Target) {
	for _, t := range rt.Targets {
		if r.breaker(t).Allow() {
			try = append(try, t)
		} else {
			skipped = append(skipped, t)
		}
	}
	if len(try) == 0 && len(rt.Targets) > 0 {
		try = []config.Target{rt.Targets[0]}
		skipped = skipped[1:]
	}
	return try, skipped
}

func (r *Router) Chat(ctx context.Context, rt config.Route, req provider.ChatRequest) (*ChatResult, error) {
	return r.chatOrStream(ctx, rt, req, nil)
}

// Stream falls back only until the first token has been sent to the client;
// after that, a failure is returned as is (the client already has a partial answer).
func (r *Router) Stream(ctx context.Context, rt config.Route, req provider.ChatRequest, emit func(provider.Delta) error) (*ChatResult, error) {
	return r.chatOrStream(ctx, rt, req, emit)
}

func (r *Router) chatOrStream(ctx context.Context, rt config.Route, req provider.ChatRequest, emit func(provider.Delta) error) (*ChatResult, error) {
	res := &ChatResult{}
	try, skipped := r.order(rt)
	for _, t := range skipped {
		res.Attempts = append(res.Attempts, Attempt{Provider: t.Provider, Model: t.Model, Skipped: true})
	}
	var last *provider.Error
	for _, t := range try {
		c, ok := r.chat[t.Provider]
		if !ok {
			return nil, fmt.Errorf("provider %q is not configured", t.Provider)
		}
		req.Model = t.Model
		start := time.Now()
		sent := false
		var resp *provider.ChatResponse
		var err error
		if emit == nil {
			resp, err = c.Chat(ctx, req)
		} else {
			resp, err = c.Stream(ctx, req, func(d provider.Delta) error {
				if !sent {
					res.TTFB = time.Since(start)
					sent = true
				}
				return emit(d)
			})
		}
		a := Attempt{Provider: t.Provider, Model: t.Model, LatencyMs: time.Since(start).Milliseconds()}
		if err == nil {
			r.breaker(t).Success()
			res.Attempts = append(res.Attempts, a)
			res.Resp, res.Target = resp, t
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err() // the client went away: not the provider's fault
		}
		pe := provider.AsError(t.Provider, err)
		a.Error, a.Kind = pe.Error(), string(pe.Kind)
		res.Attempts = append(res.Attempts, a)
		last = pe
		if countsAgainstBreaker(pe.Kind) {
			r.breaker(t).Failure(pe.Error())
		} else {
			r.breaker(t).Success() // the provider answered; the request was the problem
		}
		if sent || !pe.Retryable() || (pe.Kind == provider.KindRefused && !rt.FallbackOnRefusal) {
			break
		}
	}
	return res, &AllFailed{Attempts: res.Attempts, Last: last}
}

func countsAgainstBreaker(k provider.ErrorKind) bool {
	switch k {
	case provider.KindUnavailable, provider.KindTimeout, provider.KindRateLimited, provider.KindAuth:
		return true
	}
	return false
}

type EmbedResult struct {
	Resp     *provider.EmbedResponse
	Target   config.Target
	Attempts []Attempt
}

func (r *Router) Embed(ctx context.Context, rt config.Route, input []string) (*EmbedResult, error) {
	res := &EmbedResult{}
	try, skipped := r.order(rt)
	for _, t := range skipped {
		res.Attempts = append(res.Attempts, Attempt{Provider: t.Provider, Model: t.Model, Skipped: true})
	}
	var last *provider.Error
	for _, t := range try {
		e, ok := r.embed[t.Provider]
		if !ok {
			return nil, fmt.Errorf("provider %q has no embeddings API", t.Provider)
		}
		start := time.Now()
		resp, err := e.Embed(ctx, provider.EmbedRequest{Model: t.Model, Input: input})
		a := Attempt{Provider: t.Provider, Model: t.Model, LatencyMs: time.Since(start).Milliseconds()}
		if err == nil {
			r.breaker(t).Success()
			res.Attempts = append(res.Attempts, a)
			res.Resp, res.Target = resp, t
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pe := provider.AsError(t.Provider, err)
		a.Error, a.Kind = pe.Error(), string(pe.Kind)
		res.Attempts = append(res.Attempts, a)
		last = pe
		if countsAgainstBreaker(pe.Kind) {
			r.breaker(t).Failure(pe.Error())
		}
		if !pe.Retryable() {
			break
		}
	}
	return res, &AllFailed{Attempts: res.Attempts, Last: last}
}

type TargetStatus struct {
	Target  string        `json:"target"`
	Breaker BreakerStatus `json:"breaker"`
}

func (r *Router) Status() []TargetStatus {
	out := make([]TargetStatus, 0, len(r.breakers))
	for k, b := range r.breakers {
		out = append(out, TargetStatus{Target: k, Breaker: b.Status()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

type ModelInfo struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Targets []string `json:"targets"`
}

func (r *Router) Models() []ModelInfo {
	var out []ModelInfo
	for name, rt := range r.routes {
		m := ModelInfo{ID: name, Kind: rt.Kind}
		for _, t := range rt.Targets {
			m.Targets = append(m.Targets, t.Provider+"/"+t.Model)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
