// Command mockllm serves a fake provider for local development and
// benchmarks. Change its behaviour while it runs with POST /_mock/config.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/kuini-1/llm-gateway/internal/mockllm"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9001", "listen address")
	name := flag.String("name", "mock", "provider name shown in replies")
	latency := flag.Duration("latency", 300*time.Millisecond, "time to first token")
	jitter := flag.Duration("jitter", 100*time.Millisecond, "random extra latency, up to this much")
	tokenDelay := flag.Duration("token-delay", 10*time.Millisecond, "delay per output token")
	tokens := flag.Int("tokens", 60, "output tokens per reply")
	failRate := flag.Float64("fail-rate", 0, "share of requests that fail")
	embedLatency := flag.Duration("embed-latency", 40*time.Millisecond, "latency per embeddings request")
	flag.Parse()

	m := mockllm.New(mockllm.Config{
		Name: *name, Latency: *latency, Jitter: *jitter, TokenDelay: *tokenDelay, OutputTokens: *tokens,
		FailRate: *failRate, EmbedLatency: *embedLatency,
	})
	log.Printf("mock provider %q on %s", *name, *addr)
	log.Fatal(http.ListenAndServe(*addr, m.Handler()))
}
