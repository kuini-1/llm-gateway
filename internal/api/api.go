// Package api serves the gateway's HTTP API: OpenAI-compatible chat and
// embeddings endpoints for clients, and an admin API for the dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kuini-1/llm-gateway/internal/budget"
	"github.com/kuini-1/llm-gateway/internal/cache"
	"github.com/kuini-1/llm-gateway/internal/config"
	"github.com/kuini-1/llm-gateway/internal/pricing"
	"github.com/kuini-1/llm-gateway/internal/provider"
	"github.com/kuini-1/llm-gateway/internal/router"
	"github.com/kuini-1/llm-gateway/internal/store"
)

type Gateway struct {
	Cfg        *config.Config
	Router     *router.Router
	Prices     pricing.Table
	Store      *store.Store
	ReqLog     *store.Logger
	Budget     *budget.Budget
	Limiter    *budget.Limiter
	EmbCache   *cache.Embeddings // nil when disabled
	AdminToken string
	Dashboard  fs.FS
	OpenAPI    []byte
	Log        *slog.Logger
	Ready      func(context.Context) error
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/", http.StatusFound) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", g.readyz)
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(g.OpenAPI)
	})
	mux.HandleFunc("GET /v1/models", g.client(g.models))
	mux.HandleFunc("POST /v1/chat/completions", g.client(g.chat))
	mux.HandleFunc("POST /v1/embeddings", g.client(g.embeddings))
	g.adminRoutes(mux)
	return mux
}

func (g *Gateway) readyz(w http.ResponseWriter, r *http.Request) {
	if g.Ready != nil {
		if err := g.Ready(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", err.Error())
			return
		}
	}
	w.Write([]byte("ok"))
}

// ---- auth and limits ----

func (g *Gateway) client(next func(http.ResponseWriter, *http.Request, *store.Key)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if secret == "" || !strings.HasPrefix(secret, "gw_") {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "send a gateway key as: Authorization: Bearer gw_...")
			return
		}
		k, err := g.Store.KeyBySecret(r.Context(), secret)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "invalid_api_key", "unknown or revoked API key")
			return
		} else if err != nil {
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
			return
		}
		rpm := k.RPM
		if rpm <= 0 {
			rpm = g.Cfg.Defaults.RPM
		}
		ok, reset, err := g.Limiter.Allow(r.Context(), k.ID, rpm)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "limiter_unavailable", err.Error())
			return
		}
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(reset))
			writeError(w, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("rate limit of %d requests per minute reached", rpm))
			return
		}
		next(w, r, k)
	}
}

func scopes(k *store.Key) []budget.Scope {
	return []budget.Scope{
		{Kind: "project", ID: k.ProjectID, Cap: k.ProjectBudget},
		{Kind: "key", ID: k.ID, Cap: k.BudgetUSD},
	}
}

// ---- /v1/models ----

func (g *Gateway) models(w http.ResponseWriter, _ *http.Request, _ *store.Key) {
	var data []map[string]any
	for _, m := range g.Router.Models() {
		data = append(data, map[string]any{"id": m.ID, "object": "model", "owned_by": "gateway", "kind": m.Kind, "targets": m.Targets})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---- /v1/chat/completions ----

type chatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	Temperature         *float64 `json:"temperature"`
	Stream              bool     `json:"stream"`
	ReasoningEffort     string   `json:"reasoning_effort"`
	User                string   `json:"user"`
	// Features this gateway doesn't translate: rejected rather than ignored.
	Tools          json.RawMessage `json:"tools"`
	Functions      json.RawMessage `json:"functions"`
	ResponseFormat json.RawMessage `json:"response_format"`
	N              *int            `json:"n"`
}

func textContent(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errors.New("content must be a string or a list of text parts")
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", fmt.Errorf("content part type %q is not supported (text only)", p.Type)
		}
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request, k *store.Key) {
	start := time.Now()
	var body chatReq
	if !readJSON(w, r, &body) {
		return
	}
	switch {
	case len(body.Tools) > 0 || len(body.Functions) > 0:
		writeError(w, http.StatusBadRequest, "unsupported", "tool calling is not supported by this gateway")
		return
	case len(body.ResponseFormat) > 0:
		writeError(w, http.StatusBadRequest, "unsupported", "response_format is not supported by this gateway")
		return
	case body.N != nil && *body.N != 1:
		writeError(w, http.StatusBadRequest, "unsupported", "n must be 1")
		return
	case len(body.Messages) == 0:
		writeError(w, http.StatusBadRequest, "invalid_request", "messages is required")
		return
	}
	rt, err := g.Router.Resolve(body.Model, "chat")
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	req := provider.ChatRequest{Temperature: body.Temperature, ReasoningEffort: body.ReasoningEffort, User: body.User,
		MaxTokens: g.Cfg.Defaults.MaxTokens}
	if body.MaxCompletionTokens != nil {
		req.MaxTokens = *body.MaxCompletionTokens
	} else if body.MaxTokens != nil {
		req.MaxTokens = *body.MaxTokens
	}
	var inTok int64
	for _, m := range body.Messages {
		text, err := textContent(m.Content)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		req.Messages = append(req.Messages, provider.Message{Role: m.Role, Content: text})
		inTok += pricing.EstimateTokens(text)
	}

	// Reserve the worst case across every target this route might use.
	var reserveUSD float64
	for _, t := range rt.Targets {
		if _, ok := g.Prices[t.Model]; !ok && !g.Cfg.Defaults.AllowUnpriced {
			writeError(w, http.StatusBadRequest, "model_not_priced", fmt.Sprintf("model %q has no configured price", t.Model))
			return
		}
		reserveUSD = max(reserveUSD, g.Prices.MaxCost(t.Model, inTok+16, int64(req.MaxTokens)))
	}
	rec := &store.Request{ID: store.NewRequestID(), Time: start, ProjectID: k.ProjectID, KeyID: k.ID,
		Endpoint: "chat", Route: body.Model, Stream: body.Stream}
	res, err := g.Budget.Reserve(r.Context(), scopes(k), reserveUSD)
	if err != nil {
		g.refuseBudget(w, rec, err)
		return
	}

	var result *router.ChatResult
	var sse *sseWriter
	if body.Stream {
		sse = newSSE(w, rec.ID, body.Model)
		result, err = g.Router.Stream(r.Context(), rt, req, sse.delta)
	} else {
		result, err = g.Router.Chat(r.Context(), rt, req)
	}

	cost := 0.0
	if err == nil {
		cost, _ = g.Prices.Cost(result.Target.Model, result.Resp.Usage)
	}
	if serr := g.Budget.Settle(context.WithoutCancel(r.Context()), res, cost); serr != nil {
		g.Log.Error("settling spend failed", "request", rec.ID, "err", serr)
	}
	rec.LatencyMs = time.Since(start).Milliseconds()
	if result != nil {
		rec.Attempts, _ = json.Marshal(result.Attempts)
		rec.FallbackFrom = result.FallbackFrom()
		if body.Stream && result.TTFB > 0 {
			ms := result.TTFB.Milliseconds()
			rec.TTFBMs = &ms
		}
	}

	if err != nil {
		status, code, msg := upstreamError(err)
		if ctxErr := r.Context().Err(); ctxErr != nil {
			status, code, msg = 499, "client_closed", "the client closed the connection"
		}
		g.logError(rec, status, code, msg, result)
		if sse != nil && sse.started {
			sse.fail(code, msg)
			return
		}
		writeError(w, status, code, msg)
		return
	}

	u := result.Resp.Usage
	rec.Status, rec.HTTPStatus = "ok", http.StatusOK
	rec.Provider, rec.Model = result.Target.Provider, result.Target.Model
	rec.InputTokens, rec.OutputTokens = u.InputTokens, u.OutputTokens
	rec.CacheReadTokens, rec.CacheWriteTokens = u.CacheReadTokens, u.CacheWriteTokens
	rec.CostUSD = cost
	g.ReqLog.Log(rec)

	meta := map[string]any{
		"request_id": rec.ID, "route": body.Model, "provider": result.Target.Provider, "model": result.Target.Model,
		"cost_usd": cost, "latency_ms": rec.LatencyMs, "fallback_from": rec.FallbackFrom, "attempts": result.Attempts,
		"warnings": result.Resp.Warnings,
	}
	usage := map[string]any{
		"prompt_tokens":         u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens,
		"completion_tokens":     u.OutputTokens,
		"total_tokens":          u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens,
		"prompt_tokens_details": map[string]any{"cached_tokens": u.CacheReadTokens},
	}
	if sse != nil {
		sse.finish(result.Resp.FinishReason, usage, meta)
		return
	}
	setMetaHeaders(w, rec, result.Resp.Warnings)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": rec.ID, "object": "chat.completion", "created": start.Unix(), "model": body.Model,
		"choices": []map[string]any{{"index": 0, "finish_reason": result.Resp.FinishReason,
			"message": map[string]any{"role": "assistant", "content": result.Resp.Content}}},
		"usage": usage, "x_gateway": meta,
	})
}

func setMetaHeaders(w http.ResponseWriter, rec *store.Request, warnings []string) {
	h := w.Header()
	h.Set("X-Request-ID", rec.ID)
	h.Set("X-Gateway-Provider", rec.Provider+"/"+rec.Model)
	h.Set("X-Gateway-Cost-USD", strconv.FormatFloat(rec.CostUSD, 'f', 8, 64))
	if rec.FallbackFrom != "" {
		h.Set("X-Gateway-Fallback-From", rec.FallbackFrom)
	}
	for _, wn := range warnings {
		h.Add("X-Gateway-Warning", wn)
	}
}

func (g *Gateway) refuseBudget(w http.ResponseWriter, rec *store.Request, err error) {
	var ex *budget.ExceededError
	if errors.As(err, &ex) {
		g.logError(rec, http.StatusPaymentRequired, "budget_exceeded", ex.Error(), nil)
		// 402, not 429: clients retry 429s, and retrying won't help until the cap is raised.
		writeError(w, http.StatusPaymentRequired, "budget_exceeded", ex.Error())
		return
	}
	writeError(w, http.StatusServiceUnavailable, "budget_unavailable", err.Error())
}

func (g *Gateway) logError(rec *store.Request, status int, code, msg string, result *router.ChatResult) {
	rec.Status, rec.HTTPStatus, rec.ErrorKind, rec.Error = "error", status, code, msg
	if rec.LatencyMs == 0 {
		rec.LatencyMs = time.Since(rec.Time).Milliseconds()
	}
	if result != nil && len(result.Attempts) > 0 {
		last := result.Attempts[len(result.Attempts)-1]
		rec.Provider, rec.Model = last.Provider, last.Model
	}
	g.ReqLog.Log(rec)
}

// upstreamError maps a routing failure to the status the client sees.
func upstreamError(err error) (int, string, string) {
	var all *router.AllFailed
	if errors.As(err, &all) && all.Last == router.ErrCircuitOpen {
		return http.StatusServiceUnavailable, "circuit_open", all.Error()
	}
	if errors.As(err, &all) && all.Last != nil {
		switch all.Last.Kind {
		case provider.KindInvalid:
			return http.StatusBadRequest, "invalid_request", all.Last.Message
		case provider.KindRefused:
			return http.StatusUnprocessableEntity, "refused", all.Last.Message
		case provider.KindTimeout:
			return http.StatusGatewayTimeout, "upstream_timeout", all.Error()
		}
		return http.StatusBadGateway, "upstream_unavailable", all.Error()
	}
	return http.StatusBadGateway, "upstream_error", err.Error()
}

// ---- streaming ----

type sseWriter struct {
	w       http.ResponseWriter
	id      string
	model   string
	started bool
	created int64
}

func newSSE(w http.ResponseWriter, id, model string) *sseWriter {
	return &sseWriter{w: w, id: id, model: model, created: time.Now().Unix()}
}

// Headers go out with the first token, so a request that fails before any
// output still gets a normal JSON error with the right status.
func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Request-ID", s.id)
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseWriter) send(v any) error {
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (s *sseWriter) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
}

func (s *sseWriter) delta(d provider.Delta) error {
	s.start()
	return s.send(s.chunk(map[string]any{"content": d.Text}, nil))
}

func (s *sseWriter) finish(reason string, usage, meta map[string]any) {
	s.start()
	s.send(s.chunk(map[string]any{}, reason))
	s.send(map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{}, "usage": usage, "x_gateway": meta})
	fmt.Fprint(s.w, "data: [DONE]\n\n")
}

func (s *sseWriter) fail(code, msg string) {
	s.send(map[string]any{"error": map[string]any{"code": code, "message": msg, "type": code}})
	fmt.Fprint(s.w, "data: [DONE]\n\n")
}

// ---- /v1/embeddings ----

func (g *Gateway) embeddings(w http.ResponseWriter, r *http.Request, k *store.Key) {
	start := time.Now()
	var body struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var inputs []string
	var one string
	if json.Unmarshal(body.Input, &one) == nil {
		inputs = []string{one}
	} else if err := json.Unmarshal(body.Input, &inputs); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "input must be a string or a list of strings")
		return
	}
	if len(inputs) == 0 || len(inputs) > 2048 {
		writeError(w, http.StatusBadRequest, "invalid_request", "input must have 1 to 2048 items")
		return
	}
	rt, err := g.Router.Resolve(body.Model, "embedding")
	if err != nil {
		writeError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	model := rt.Targets[0].Model // every target of an embedding route serves this model
	rec := &store.Request{ID: store.NewRequestID(), Time: start, ProjectID: k.ProjectID, KeyID: k.ID,
		Endpoint: "embeddings", Route: body.Model, EmbInputs: len(inputs)}

	vecs := make([][]float32, len(inputs))
	var hitTokens int64
	var miss []int
	if g.EmbCache != nil {
		cached, err := g.EmbCache.Get(r.Context(), model, inputs)
		if err != nil {
			g.Log.Warn("embedding cache read failed; continuing without it", "err", err)
			cached = make([]*cache.Entry, len(inputs))
		}
		for i, e := range cached {
			if e != nil {
				vecs[i] = e.Vector
				hitTokens += e.Tokens
			} else {
				miss = append(miss, i)
			}
		}
	} else {
		for i := range inputs {
			miss = append(miss, i)
		}
	}
	rec.EmbCacheHits = len(inputs) - len(miss)
	rec.SavedUSD = g.Prices.EmbedCost(model, hitTokens)

	var billed int64
	var result *router.EmbedResult
	if len(miss) > 0 {
		texts := make([]string, len(miss))
		var est int64
		for j, i := range miss {
			texts[j] = inputs[i]
			est += pricing.EstimateTokens(inputs[i])
		}
		res, err := g.Budget.Reserve(r.Context(), scopes(k), g.Prices.EmbedCost(model, est*2+16))
		if err != nil {
			g.refuseBudget(w, rec, err)
			return
		}
		result, err = g.Router.Embed(r.Context(), rt, texts)
		if err == nil {
			billed = result.Resp.Tokens
			rec.CostUSD = g.Prices.EmbedCost(model, billed)
		}
		g.Budget.Settle(context.WithoutCancel(r.Context()), res, rec.CostUSD)
		if err != nil {
			status, code, msg := upstreamError(err)
			rec.Status, rec.HTTPStatus, rec.ErrorKind, rec.Error = "error", status, code, msg
			rec.LatencyMs = time.Since(start).Milliseconds()
			if result != nil {
				rec.Attempts, _ = json.Marshal(result.Attempts)
			}
			g.ReqLog.Log(rec)
			writeError(w, status, code, msg)
			return
		}
		entries := make([]cache.Entry, len(miss))
		share := tokenShares(texts, billed)
		for j, i := range miss {
			vecs[i] = result.Resp.Vectors[j]
			entries[j] = cache.Entry{Vector: vecs[i], Tokens: share[j]}
		}
		if g.EmbCache != nil {
			if err := g.EmbCache.Put(context.WithoutCancel(r.Context()), model, texts, entries); err != nil {
				g.Log.Warn("embedding cache write failed", "err", err)
			}
		}
		rec.Provider, rec.Model = result.Target.Provider, result.Target.Model
		rec.Attempts, _ = json.Marshal(result.Attempts)
		rec.FallbackFrom = result.FallbackFrom()
	} else {
		rec.Provider, rec.Model = "cache", model
	}

	rec.Status, rec.HTTPStatus, rec.InputTokens = "ok", http.StatusOK, billed
	rec.LatencyMs = time.Since(start).Milliseconds()
	g.ReqLog.Log(rec)

	data := make([]map[string]any, len(inputs))
	for i, v := range vecs {
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": v}
	}
	setMetaHeaders(w, rec, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "model": body.Model, "data": data,
		"usage": map[string]any{"prompt_tokens": billed, "total_tokens": billed},
		"x_gateway": map[string]any{"request_id": rec.ID, "provider": rec.Provider, "model": model,
			"cache_hits": rec.EmbCacheHits, "cache_misses": len(miss), "cost_usd": rec.CostUSD, "saved_usd": rec.SavedUSD},
	})
}

// tokenShares splits a batch's billed tokens across its inputs by length,
// so each cached vector remembers roughly what it cost.
func tokenShares(texts []string, total int64) []int64 {
	out := make([]int64, len(texts))
	var chars int64
	for _, t := range texts {
		chars += int64(len(t)) + 1
	}
	var given int64
	for i, t := range texts {
		out[i] = total * (int64(len(t)) + 1) / chars
		given += out[i]
	}
	if len(out) > 0 {
		out[len(out)-1] += total - given // rounding remainder
	}
	return out
}

// ---- helpers ----

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError uses the OpenAI error shape, so existing client libraries
// surface the message.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}})
}
