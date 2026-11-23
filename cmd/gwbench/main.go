// Command gwbench measures the gateway against mock providers: the latency
// it adds, how it behaves when a provider goes down, and what the embedding
// cache saves on a re-indexing workload. With real API keys, "providers"
// compares real provider latency through the gateway.
//
// It expects a running gateway configured with bench/gateway.bench.yaml and
// the mock providers that config points to (see bench/README.md).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	gatewayURL = flag.String("gateway", "http://127.0.0.1:8080", "gateway URL")
	adminToken = flag.String("admin-token", os.Getenv("GATEWAY_ADMIN_TOKEN"), "admin token (creates the bench project and key)")
	mockA      = flag.String("mock-primary", "http://127.0.0.1:9001", "primary mock (Anthropic API)")
	mockB      = flag.String("mock-secondary", "http://127.0.0.1:9002", "secondary mock (OpenAI API)")
	out        = flag.String("out", "bench/results", "output folder")
	client     = &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gwbench [flags] overhead|outage|cache|all|providers ROUTE...")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	key, err := benchKey(ctx)
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	var sections []string
	run := func(name string, f func(context.Context, string) (string, error)) {
		fmt.Printf("== %s\n", name)
		md, err := f(ctx, key)
		if err != nil {
			fail(fmt.Errorf("%s: %w", name, err))
		}
		fmt.Println(md)
		sections = append(sections, md)
	}
	switch flag.Arg(0) {
	case "overhead":
		run("overhead", overhead)
	case "outage":
		run("outage", outage)
	case "cache":
		run("cache", cacheBench)
	case "all":
		run("overhead", overhead)
		run("outage", outage)
		run("cache", cacheBench)
	case "providers":
		run("providers", func(ctx context.Context, key string) (string, error) { return providers(ctx, key, flag.Args()[1:]) })
	default:
		flag.Usage()
		os.Exit(2)
	}
	report := fmt.Sprintf("# Gateway benchmarks\n\nRun on %s with `gwbench %s`.\n\n%s", time.Now().Format("2006-01-02"),
		strings.Join(flag.Args(), " "), strings.Join(sections, "\n"))
	path := *out + "/" + flag.Arg(0) + ".md"
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		fail(err)
	}
	fmt.Println("wrote", path)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// ---- setup ----

func adminDo(ctx context.Context, method, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, method, *gatewayURL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+*adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// benchKey creates a project and key with a high rate limit for the run.
func benchKey(ctx context.Context) (string, error) {
	var p struct {
		ID string `json:"id"`
	}
	name := "bench-" + time.Now().Format("20060102-150405")
	if err := adminDo(ctx, "POST", "/admin/api/projects", map[string]any{"name": name}, &p); err != nil {
		return "", err
	}
	var k struct {
		Secret string `json:"secret"`
	}
	err := adminDo(ctx, "POST", "/admin/api/keys", map[string]any{"project_id": p.ID, "name": "gwbench", "rpm": 1_000_000}, &k)
	return k.Secret, err
}

func setMock(ctx context.Context, base string, cfg map[string]any) error {
	b, _ := json.Marshal(cfg)
	req, _ := http.NewRequestWithContext(ctx, "POST", base+"/_mock/config", bytes.NewReader(b))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func mockRequests(ctx context.Context, base string) int64 {
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/_mock/stats", nil)
	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	var s map[string]int64
	json.NewDecoder(resp.Body).Decode(&s)
	return s["requests"]
}

// ---- load generation ----

type result struct {
	latency  time.Duration
	ttfb     time.Duration
	status   int
	fallback bool
}

type sample struct {
	mu      sync.Mutex
	results []result
}

func (s *sample) add(r result) {
	s.mu.Lock()
	s.results = append(s.results, r)
	s.mu.Unlock()
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[min(len(ds)-1, int(float64(len(ds))*p))]
}

func (s *sample) stats() (ok int, p50, p95, p99 time.Duration, ttfb50 time.Duration, fallbacks int) {
	var lat, tt []time.Duration
	for _, r := range s.results {
		if r.status == 200 {
			ok++
			lat = append(lat, r.latency)
			if r.ttfb > 0 {
				tt = append(tt, r.ttfb)
			}
			if r.fallback {
				fallbacks++
			}
		}
	}
	return ok, pct(lat, 0.5), pct(lat, 0.95), pct(lat, 0.99), pct(tt, 0.5), fallbacks
}

// load sends n requests with the given concurrency.
func load(ctx context.Context, n, conc int, do func(i int) result) (*sample, time.Duration) {
	s := &sample{}
	var next atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n || ctx.Err() != nil {
					return
				}
				s.add(do(i))
			}
		}()
	}
	wg.Wait()
	return s, time.Since(start)
}

func chatOnce(ctx context.Context, url, key, model string, stream bool) result {
	body := map[string]any{"model": model, "max_tokens": 200, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": "Summarise the benefits of a gateway in one paragraph."}}}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return result{status: -1, latency: time.Since(start)}
	}
	defer resp.Body.Close()
	r := result{status: resp.StatusCode, fallback: resp.Header.Get("X-Gateway-Fallback-From") != ""}
	if stream && resp.StatusCode == 200 {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if r.ttfb == 0 && strings.Contains(line, `"content"`) {
				r.ttfb = time.Since(start)
			}
			if strings.Contains(line, `"x_gateway"`) && strings.Contains(line, `"fallback_from":"`) && !strings.Contains(line, `"fallback_from":""`) {
				r.fallback = true
			}
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	r.latency = time.Since(start)
	return r
}

func f(d time.Duration) string {
	if d >= time.Second {
		return fmt.Sprintf("%.2f s", d.Seconds())
	}
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

// ---- 1. overhead ----

func overhead(ctx context.Context, key string) (string, error) {
	// Fixed upstream latency, no jitter, so differences are the gateway's.
	cfg := map[string]any{"name": "openai-mock", "latency": int64(50 * time.Millisecond), "jitter": 0, "token_delay": 0,
		"output_tokens": 60, "fail_rate": 0}
	if err := setMock(ctx, *mockB, cfg); err != nil {
		return "", err
	}
	const n, conc = 2000, 32
	// Warm up connections on both paths.
	load(ctx, 100, conc, func(int) result { return chatOnce(ctx, *mockB+"/v1/chat/completions", "", "gpt-bench", false) })
	load(ctx, 100, conc, func(int) result { return chatOnce(ctx, *gatewayURL+"/v1/chat/completions", key, "bench-direct", false) })

	type row struct {
		name string
		s    *sample
		dur  time.Duration
	}
	var rows []row
	for _, c := range []struct {
		name, url, model, key string
		stream                bool
	}{
		{"direct to provider", *mockB + "/v1/chat/completions", "gpt-bench", "", false},
		{"through the gateway", *gatewayURL + "/v1/chat/completions", "bench-direct", key, false},
		{"direct, streaming", *mockB + "/v1/chat/completions", "gpt-bench", "", true},
		{"gateway, streaming", *gatewayURL + "/v1/chat/completions", "bench-direct", key, true},
	} {
		s, d := load(ctx, n, conc, func(int) result { return chatOnce(ctx, c.url, c.key, c.model, c.stream) })
		rows = append(rows, row{c.name, s, d})
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Gateway overhead\n\n%d chat requests per row, %d concurrent, against a mock provider with a fixed 50 ms response time. Each gateway request authenticates the key, checks the rate limit, reserves and settles spend in Redis and queues a log row for Postgres.\n\n", n, conc)
	sb.WriteString("| Path | OK | p50 | p95 | p99 | First token p50 | Throughput |\n|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		ok, p50, p95, p99, tt, _ := r.s.stats()
		ttS := "–"
		if tt > 0 {
			ttS = f(tt)
		}
		fmt.Fprintf(&sb, "| %s | %d/%d | %s | %s | %s | %s | %.0f req/s |\n", r.name, ok, n, f(p50), f(p95), f(p99), ttS, float64(n)/r.dur.Seconds())
	}
	_, d50, d95, _, _, _ := rows[0].s.stats()
	_, g50, g95, _, _, _ := rows[1].s.stats()
	fmt.Fprintf(&sb, "\nAdded latency: **%s at p50, %s at p95** (non-streaming).\n", f(g50-d50), f(g95-d95))
	return sb.String(), nil
}

// ---- 2. outage ----

func outage(ctx context.Context, key string) (string, error) {
	normal := map[string]any{"name": "claude-mock", "latency": int64(150 * time.Millisecond), "jitter": int64(50 * time.Millisecond),
		"token_delay": 0, "output_tokens": 60, "fail_rate": 0}
	down := map[string]any{"name": "claude-mock", "latency": int64(150 * time.Millisecond), "jitter": int64(50 * time.Millisecond),
		"token_delay": 0, "output_tokens": 60, "fail_rate": 1, "fail_status": 503}
	secondary := map[string]any{"name": "openai-mock", "latency": int64(200 * time.Millisecond), "jitter": int64(50 * time.Millisecond),
		"token_delay": 0, "output_tokens": 60, "fail_rate": 0}
	if err := setMock(ctx, *mockB, secondary); err != nil {
		return "", err
	}

	type phase struct {
		name    string
		route   string
		primary map[string]any
	}
	// Order matters and is part of the result: the second row starts with a
	// fresh breaker and shows how many calls it takes to open; the third row
	// uses the same primary target, whose breaker is then already open.
	phases := []phase{
		{"healthy", "bench-fallback", normal},
		{"primary down, fallback route", "bench-fallback", down},
		{"primary down, single-target route (circuit already open)", "bench-primary-only", down},
	}
	const n, conc = 600, 16
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Provider outage\n\n%d requests per row, %d concurrent. The primary mock answers in 150 ms ± 50 ms, the secondary in 200 ms ± 50 ms. \"Down\" means every primary request returns 503. The circuit breaker opens after 5 consecutive failures and probes again after 30 s. Latency is over all requests, failed ones included.\n\n", n, conc)
	sb.WriteString("| Scenario | Succeeded | Served by the fallback | p50 | p95 | Calls that reached the dead primary |\n|---|---|---|---|---|---|\n")
	for _, ph := range phases {
		if err := setMock(ctx, *mockA, ph.primary); err != nil {
			return "", err
		}
		before := mockRequests(ctx, *mockA)
		s, _ := load(ctx, n, conc, func(int) result {
			return chatOnce(ctx, *gatewayURL+"/v1/chat/completions", key, ph.route, false)
		})
		hit := mockRequests(ctx, *mockA) - before
		ok, _, _, _, _, fb := s.stats()
		var all []time.Duration
		for _, r := range s.results {
			all = append(all, r.latency)
		}
		deadHits := "–"
		if ph.primary["fail_rate"] == 1 {
			deadHits = fmt.Sprintf("%d of %d", hit, n)
		}
		fmt.Fprintf(&sb, "| %s | %d/%d (%.1f%%) | %d | %s | %s | %s |\n", ph.name, ok, n, 100*float64(ok)/n, fb,
			f(pct(all, 0.5)), f(pct(append([]time.Duration(nil), all...), 0.95)), deadHits)
	}
	if err := setMock(ctx, *mockA, normal); err != nil {
		return "", err
	}
	sb.WriteString("\nOnce the breaker opens, the fallback route sends requests straight to the secondary instead of paying for a failed call first, and the single-target route fails fast with 503 `circuit_open` instead of waiting on a provider that is down.\n")
	return sb.String(), nil
}

// ---- 3. embedding cache ----

const embedPricePerM = 0.02 // USD per million tokens, the price configured for bench-embed

func cacheBench(ctx context.Context, key string) (string, error) {
	if err := setMock(ctx, *mockB, map[string]any{"name": "openai-mock", "latency": 0, "jitter": 0, "embed_latency": int64(40 * time.Millisecond), "fail_rate": 0}); err != nil {
		return "", err
	}
	rng := rand.New(rand.NewPCG(7, 42))
	runID := time.Now().UnixNano() // fresh texts, so earlier runs don't warm the cache
	chunks := make([]string, 400)
	for i := range chunks {
		chunks[i] = synthText(rng, fmt.Sprintf("doc-%d chunk %d run %d", i/20, i, runID), 110)
	}
	queries := make([]string, 300)
	for i := range queries {
		queries[i] = synthText(rng, fmt.Sprintf("q%d run %d", i, runID), 12)
	}

	type phaseStats struct {
		name           string
		inputs, hits   int
		billed, saved  float64
		p50Hit, p50New time.Duration
	}
	var phases []phaseStats
	embed := func(name string, texts []string, batch int) error {
		ps := phaseStats{name: name}
		var hitLat, missLat []time.Duration
		for i := 0; i < len(texts); i += batch {
			b := texts[i:min(len(texts), i+batch)]
			start := time.Now()
			var resp struct {
				XGW struct {
					Hits   int     `json:"cache_hits"`
					Misses int     `json:"cache_misses"`
					Cost   float64 `json:"cost_usd"`
					Saved  float64 `json:"saved_usd"`
				} `json:"x_gateway"`
			}
			if err := post(ctx, key, "/v1/embeddings", map[string]any{"model": "bench-embed", "input": b}, &resp); err != nil {
				return err
			}
			d := time.Since(start)
			if resp.XGW.Misses == 0 {
				hitLat = append(hitLat, d)
			} else if resp.XGW.Hits == 0 {
				missLat = append(missLat, d)
			}
			ps.inputs += len(b)
			ps.hits += resp.XGW.Hits
			ps.billed += resp.XGW.Cost
			ps.saved += resp.XGW.Saved
		}
		ps.p50Hit, ps.p50New = pct(hitLat, 0.5), pct(missLat, 0.5)
		phases = append(phases, ps)
		return nil
	}

	// Initial index, then three re-indexing passes where 10% of chunks changed.
	if err := embed("initial index (400 chunks)", chunks, 32); err != nil {
		return "", err
	}
	for pass := 1; pass <= 3; pass++ {
		for j := 0; j < 40; j++ {
			i := rng.IntN(len(chunks))
			chunks[i] = synthText(rng, fmt.Sprintf("doc-%d chunk %d edit %d-%d run %d", i/20, i, pass, j, runID), 110)
		}
		if err := embed(fmt.Sprintf("re-index %d (10%% of chunks edited)", pass), chunks, 32); err != nil {
			return "", err
		}
	}
	// Search traffic: 2,000 queries drawn from 300 distinct ones with a Zipf skew.
	zipf := rand.NewZipf(rand.New(rand.NewPCG(1, 2)), 1.1, 1, uint64(len(queries)-1))
	var qs []string
	for i := 0; i < 2000; i++ {
		qs = append(qs, queries[zipf.Uint64()])
	}
	if err := embed("2,000 search queries (300 distinct, Zipf)", qs, 1); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("## Embedding cache\n\nA synthetic RAG workload through the `bench-embed` route: index 400 chunks of about 110 words, re-index three times with 10% of chunks edited each time, then serve 2,000 single-text search queries drawn from 300 distinct queries with a Zipf (s = 1.1) skew, so a few queries are very common. Cost uses $0.02 per million tokens, the price configured for this route.\n\n")
	sb.WriteString("| Phase | Inputs | Cache hits | Paid | Saved | p50 all hits | p50 all misses |\n|---|---|---|---|---|---|---|\n")
	var inputs, hits int
	var billed, saved float64
	for _, p := range phases {
		inputs += p.inputs
		hits += p.hits
		billed += p.billed
		saved += p.saved
		lat := func(d time.Duration) string {
			if d == 0 {
				return "–"
			}
			return f(d)
		}
		fmt.Fprintf(&sb, "| %s | %d | %d (%.0f%%) | $%.6f | $%.6f | %s | %s |\n", p.name, p.inputs, p.hits,
			100*float64(p.hits)/float64(p.inputs), p.billed, p.saved, lat(p.p50Hit), lat(p.p50New))
	}
	fmt.Fprintf(&sb, "| **total** | %d | %d (%.0f%%) | $%.6f | $%.6f | | |\n", inputs, hits, 100*float64(hits)/float64(inputs), billed, saved)
	share := saved / (billed + saved)
	fmt.Fprintf(&sb, "\nThe cache cut embedding spend by **%.0f%%** on this workload ($%.6f paid instead of $%.6f).\n", 100*share, billed, billed+saved)
	return sb.String(), nil
}

var words = strings.Fields(`account access api archive backup bucket cache capacity change client cluster config cost data
deploy disk endpoint error event file flow gateway index key latency limit log metric model network node object
plan policy price project quota queue rate region replica request resource retry route schema search secret
service session shard snapshot storage stream tenant token traffic upload usage user version volume worker zone`)

func synthText(rng *rand.Rand, prefix string, n int) string {
	var sb strings.Builder
	sb.WriteString(prefix)
	for i := 0; i < n; i++ {
		sb.WriteByte(' ')
		sb.WriteString(words[rng.IntN(len(words))])
	}
	return sb.String()
}

func post(ctx context.Context, key, path string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", *gatewayURL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, msg)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- 4. real providers ----

func providers(ctx context.Context, key string, routes []string) (string, error) {
	if len(routes) == 0 {
		return "", errors.New("name the routes to compare, e.g. gwbench providers claude gpt deepseek")
	}
	const n, conc = 30, 3
	var sb strings.Builder
	fmt.Fprintf(&sb, "## Provider latency\n\n%d streaming requests per route, %d concurrent, same prompt, max_tokens 200.\n\n", n, conc)
	sb.WriteString("| Route | OK | First token p50 | First token p95 | Total p50 | Total p95 |\n|---|---|---|---|---|---|\n")
	for _, r := range routes {
		s, _ := load(ctx, n, conc, func(int) result { return chatOnce(ctx, *gatewayURL+"/v1/chat/completions", key, r, true) })
		ok, p50, p95, _, tt50, _ := s.stats()
		var tts []time.Duration
		for _, x := range s.results {
			if x.ttfb > 0 {
				tts = append(tts, x.ttfb)
			}
		}
		fmt.Fprintf(&sb, "| %s | %d/%d | %s | %s | %s | %s |\n", r, ok, n, f(tt50), f(pct(tts, 0.95)), f(p50), f(p95))
	}
	return sb.String(), nil
}
