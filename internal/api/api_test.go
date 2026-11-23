package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kuini-1/llm-gateway/internal/budget"
	"github.com/kuini-1/llm-gateway/internal/cache"
	"github.com/kuini-1/llm-gateway/internal/config"
	"github.com/kuini-1/llm-gateway/internal/mockllm"
	"github.com/kuini-1/llm-gateway/internal/pricing"
	"github.com/kuini-1/llm-gateway/internal/router"
	"github.com/kuini-1/llm-gateway/internal/store"
)

type env struct {
	t         *testing.T
	url       string
	store     *store.Store
	log       *store.Logger
	claude    *mockllm.Server
	openai    *mockllm.Server
	projectID string
	key       string
}

// newEnv needs Postgres and Redis:
//
//	GATEWAY_TEST_POSTGRES=postgres://gateway:gateway@127.0.0.1:5442/gateway_test
//	GATEWAY_TEST_REDIS=redis://127.0.0.1:6389/15
func newEnv(t *testing.T) *env {
	t.Helper()
	pgURL, redisURL := os.Getenv("GATEWAY_TEST_POSTGRES"), os.Getenv("GATEWAY_TEST_REDIS")
	if pgURL == "" || redisURL == "" {
		t.Skip("set GATEWAY_TEST_POSTGRES and GATEWAY_TEST_REDIS")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(ctx, "TRUNCATE requests, api_keys, projects"); err != nil {
		t.Fatal(err)
	}
	opt, _ := redis.ParseURL(redisURL)
	rdb := redis.NewClient(opt)
	rdb.FlushDB(ctx)

	claude := mockllm.New(mockllm.Config{Name: "claude-mock", OutputTokens: 20})
	openai := mockllm.New(mockllm.Config{Name: "openai-mock", OutputTokens: 20, EmbedDim: 8})
	cs, os_ := httptest.NewServer(claude.Handler()), httptest.NewServer(openai.Handler())

	cfg := &config.Config{
		Providers: map[string]config.Provider{
			"anthropic": {Kind: "anthropic", BaseURL: cs.URL, APIKey: "test", Timeout: 5 * time.Second},
			"openai":    {Kind: "openai", BaseURL: os_.URL + "/v1", APIKey: "test", Timeout: 5 * time.Second},
		},
		Routes: map[string]config.Route{
			"smart": {Kind: "chat", Targets: []config.Target{{Provider: "anthropic", Model: "claude-opus-5-5"}, {Provider: "openai", Model: "gpt-test"}}},
			"embed": {Kind: "embedding", Targets: []config.Target{{Provider: "openai", Model: "embed-test"}}},
		},
		Prices: map[string]config.Price{
			"claude-opus-5-5": {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
			"gpt-test":        {Input: 1, Output: 4},
			"embed-test":      {Input: 0.02},
		},
		Breaker:  config.Breaker{FailureThreshold: 100, OpenFor: time.Minute},
		Defaults: config.Defaults{MaxTokens: 1000, RPM: 1000},
	}
	reqLog := store.NewLogger(st)
	g := &Gateway{
		Cfg: cfg, Router: router.New(cfg), Prices: pricing.Table(cfg.Prices), Store: st, ReqLog: reqLog,
		Budget:     &budget.Budget{RDB: rdb, Load: st.MonthSpend},
		Limiter:    &budget.Limiter{RDB: rdb},
		EmbCache:   &cache.Embeddings{RDB: rdb, TTL: time.Hour},
		AdminToken: "admin-secret", OpenAPI: []byte("openapi: 3.1.0"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(func() { srv.Close(); cs.Close(); os_.Close(); rdb.Close(); st.Close() })

	p, err := st.CreateProject(ctx, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreateKey(ctx, p.ID, "tester", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, url: srv.URL, store: st, log: reqLog, claude: claude, openai: openai, projectID: p.ID, key: secret}
}

func (e *env) do(method, path, key string, body any) (*http.Response, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func chatBody(model string) map[string]any {
	return map[string]any{"model": model, "max_tokens": 100, "messages": []map[string]any{
		{"role": "system", "content": "Be brief."}, {"role": "user", "content": "Hello gateway"}}}
}

func TestChatFallbackStreamAndLog(t *testing.T) {
	e := newEnv(t)

	// Normal call: served by the first target, priced, OpenAI-shaped.
	resp, out := e.do("POST", "/v1/chat/completions", e.key, chatBody("smart"))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	gw := out["x_gateway"].(map[string]any)
	if !strings.Contains(msg, "claude-mock") || gw["provider"] != "anthropic" || gw["cost_usd"].(float64) <= 0 {
		t.Fatalf("unexpected answer: %v %v", msg, gw)
	}
	if resp.Header.Get("X-Gateway-Provider") != "anthropic/claude-opus-5-5" {
		t.Errorf("provider header %q", resp.Header.Get("X-Gateway-Provider"))
	}

	// Primary down: the same request is answered by OpenAI and says so.
	e.claude.SetConfig(mockllm.Config{Name: "claude-mock", FailRate: 1, FailStatus: 529, OutputTokens: 20})
	resp, out = e.do("POST", "/v1/chat/completions", e.key, chatBody("smart"))
	if resp.StatusCode != 200 || resp.Header.Get("X-Gateway-Fallback-From") != "anthropic/claude-opus-5-5" {
		t.Fatalf("fallback: %d %v", resp.StatusCode, out)
	}

	// Streaming through the fallback target.
	body := chatBody("smart")
	body["stream"] = true
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.url+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.key)
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var sawUsage, sawDone bool
	sc := bufio.NewScanner(sresp.Body)
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "data: ")
		if line == "[DONE]" {
			sawDone = true
			break
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(line), &chunk) != nil {
			continue
		}
		if chunk["usage"] != nil {
			sawUsage = chunk["x_gateway"].(map[string]any)["provider"] == "openai"
		}
		for _, c := range chunk["choices"].([]any) {
			if d, ok := c.(map[string]any)["delta"].(map[string]any)["content"].(string); ok {
				text.WriteString(d)
			}
		}
	}
	sresp.Body.Close()
	if !strings.Contains(text.String(), "openai-mock") || !sawUsage || !sawDone {
		t.Fatalf("stream: text %q usage %v done %v", text.String(), sawUsage, sawDone)
	}

	// Invalid requests fail fast and don't fall back.
	before := e.openai.Requests.Load()
	e.claude.SetConfig(mockllm.Config{Name: "claude-mock", OutputTokens: 20})
	resp, _ = e.do("POST", "/v1/chat/completions", e.key, map[string]any{"model": "smart", "messages": []any{}})
	if resp.StatusCode != 400 || e.openai.Requests.Load() != before {
		t.Fatalf("empty messages: %d", resp.StatusCode)
	}
	resp, _ = e.do("POST", "/v1/chat/completions", e.key, map[string]any{"model": "smart", "tools": []any{map[string]any{}},
		"messages": []any{map[string]any{"role": "user", "content": "x"}}})
	if resp.StatusCode != 400 {
		t.Fatalf("tools should be rejected, got %d", resp.StatusCode)
	}
	if resp, _ = e.do("POST", "/v1/chat/completions", e.key, chatBody("openai/unpriced-model")); resp.StatusCode != 400 {
		t.Fatalf("unpriced model: %d", resp.StatusCode)
	}

	// Every request lands in the log, with the fallback recorded.
	e.log.Close()
	rows, err := e.store.RecentRequests(context.Background(), "fallback", 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("fallback rows = %d, %v", len(rows), err)
	}
	stats, _ := e.store.Stats(context.Background(), time.Now().Add(-time.Hour), "minute")
	if stats.Totals.Requests < 3 || stats.Totals.CostUSD <= 0 || stats.Totals.Fallbacks != 2 {
		t.Errorf("stats = %+v", stats.Totals)
	}
}

func TestEmbeddingCache(t *testing.T) {
	e := newEnv(t)
	in := map[string]any{"model": "embed", "input": []string{"alpha", "beta"}}
	_, first := e.do("POST", "/v1/embeddings", e.key, in)
	if n := len(first["data"].([]any)); n != 2 {
		t.Fatalf("got %d vectors", n)
	}
	gw := first["x_gateway"].(map[string]any)
	if gw["cache_hits"].(float64) != 0 || gw["cost_usd"].(float64) <= 0 {
		t.Fatalf("first call: %v", gw)
	}
	// Same inputs plus one new: two hits, one miss, only the new text goes upstream.
	upstream := e.openai.Embedded.Load()
	_, second := e.do("POST", "/v1/embeddings", e.key, map[string]any{"model": "embed", "input": []string{"beta", "gamma", "alpha"}})
	gw = second["x_gateway"].(map[string]any)
	if gw["cache_hits"].(float64) != 2 || gw["saved_usd"].(float64) <= 0 || e.openai.Embedded.Load()-upstream != 1 {
		t.Fatalf("second call: %v, upstream embedded %d", gw, e.openai.Embedded.Load()-upstream)
	}
	// Cached vectors are identical to fresh ones and returned in input order.
	v1 := first["data"].([]any)[0].(map[string]any)["embedding"].([]any)
	v2 := second["data"].([]any)[2].(map[string]any)["embedding"].([]any)
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatal("cached vector differs from the original")
		}
	}
}

func TestAuthLimitsAndBudget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if resp, _ := e.do("POST", "/v1/chat/completions", "", chatBody("smart")); resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	if resp, _ := e.do("POST", "/v1/chat/completions", "gw_wrong", chatBody("smart")); resp.StatusCode != 401 {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}

	// A key with rpm 2: the third request in the minute is refused.
	_, limited, _ := e.store.CreateKey(ctx, e.projectID, "limited", 0, 2)
	codes := []int{}
	for i := 0; i < 3; i++ {
		resp, _ := e.do("GET", "/v1/models", limited, nil)
		codes = append(codes, resp.StatusCode)
	}
	if codes[2] != 429 {
		t.Fatalf("rate limit codes %v", codes)
	}

	// A $0.003 cap. A request whose worst case (1,000 output tokens of Opus = $0.02)
	// can't fit is refused before any upstream call.
	p, _ := e.store.CreateProject(ctx, "capped", 0.003)
	_, capped, _ := e.store.CreateKey(ctx, p.ID, "capped", 0, 0)
	big := chatBody("smart")
	big["max_tokens"] = 1000
	upstream := e.claude.Requests.Load()
	resp, out := e.do("POST", "/v1/chat/completions", capped, big)
	if resp.StatusCode != 402 || out["error"].(map[string]any)["code"] != "budget_exceeded" || e.claude.Requests.Load() != upstream {
		t.Fatalf("worst case over cap: %d %v", resp.StatusCode, out)
	}
	// Small requests ($0.002 reserved, about $0.0004 actual) fit until real spend fills the cap.
	ok, refusedAt := 0, 0
	for i := 1; i <= 10 && refusedAt == 0; i++ {
		if resp, _ := e.do("POST", "/v1/chat/completions", capped, chatBody("smart")); resp.StatusCode == 200 {
			ok++
		} else if resp.StatusCode == 402 {
			refusedAt = i
		}
	}
	if ok < 2 || refusedAt == 0 {
		t.Fatalf("cap: %d succeeded, refused at %d", ok, refusedAt)
	}

	// Revoked keys stop working (the key cache is flushed on revoke).
	keys, _ := e.store.Keys(ctx)
	for _, k := range keys {
		if k.Name == "capped" {
			e.store.RevokeKey(ctx, k.ID)
		}
	}
	if resp, _ := e.do("GET", "/v1/models", capped, nil); resp.StatusCode != 401 {
		t.Fatalf("revoked key: %d", resp.StatusCode)
	}

	// Admin API needs the admin token.
	if resp, _ := e.do("GET", "/admin/api/overview", e.key, nil); resp.StatusCode != 401 {
		t.Fatalf("admin with client key: %d", resp.StatusCode)
	}
	resp, out = e.do("GET", "/admin/api/overview", "admin-secret", nil)
	if resp.StatusCode != 200 || out["projects"] == nil {
		t.Fatalf("admin overview: %d %v", resp.StatusCode, out)
	}
}
