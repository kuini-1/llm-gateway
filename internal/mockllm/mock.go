// Package mockllm is a fake model provider for tests and benchmarks. It
// serves the Anthropic Messages API and the OpenAI chat and embeddings APIs
// closely enough for the official clients, with configurable latency,
// failure rate and refusals. Behaviour can be changed at runtime through
// POST /_mock/config, which is how the benchmarks simulate an outage.
package mockllm

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Name         string        `json:"name"`
	Latency      time.Duration `json:"latency"`       // time to first token
	Jitter       time.Duration `json:"jitter"`        // added uniformly at random, 0..Jitter
	TokenDelay   time.Duration `json:"token_delay"`   // between streamed tokens (and per output token when not streaming)
	OutputTokens int           `json:"output_tokens"` // length of every reply
	FailRate     float64       `json:"fail_rate"`     // share of requests that fail
	FailStatus   int           `json:"fail_status"`   // status for failures (default 503)
	RefuseRate   float64       `json:"refuse_rate"`   // Anthropic only: stop_reason "refusal"
	EmbedDim     int           `json:"embed_dim"`     // embedding size
	EmbedLatency time.Duration `json:"embed_latency"` // per embeddings request
}

type Server struct {
	mu       sync.RWMutex
	cfg      Config
	Requests atomic.Int64
	Failures atomic.Int64
	Embedded atomic.Int64 // inputs embedded
}

func New(cfg Config) *Server {
	if cfg.OutputTokens == 0 {
		cfg.OutputTokens = 50
	}
	if cfg.FailStatus == 0 {
		cfg.FailStatus = http.StatusServiceUnavailable
	}
	if cfg.EmbedDim == 0 {
		cfg.EmbedDim = 256
	}
	return &Server{cfg: cfg}
}

func (s *Server) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) SetConfig(c Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.FailStatus == 0 {
		c.FailStatus = http.StatusServiceUnavailable
	}
	s.cfg = c
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.anthropic)
	mux.HandleFunc("POST /v1/chat/completions", s.openaiChat)
	mux.HandleFunc("POST /chat/completions", s.openaiChat)
	mux.HandleFunc("POST /v1/embeddings", s.embeddings)
	mux.HandleFunc("POST /embeddings", s.embeddings)
	mux.HandleFunc("GET /_mock/config", func(w http.ResponseWriter, _ *http.Request) { json.NewEncoder(w).Encode(s.Config()) })
	mux.HandleFunc("POST /_mock/config", func(w http.ResponseWriter, r *http.Request) {
		c := s.Config()
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetConfig(c)
		json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("GET /_mock/stats", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]int64{"requests": s.Requests.Load(), "failures": s.Failures.Load(), "embedded": s.Embedded.Load()})
	})
	return mux
}

// ---- shared ----

func (s *Server) begin(w http.ResponseWriter, r *http.Request, delay time.Duration) (Config, bool) {
	s.Requests.Add(1)
	c := s.Config()
	d := delay
	if c.Jitter > 0 {
		d += time.Duration(rand.Int64N(int64(c.Jitter)))
	}
	select {
	case <-time.After(d):
	case <-r.Context().Done():
		return c, false
	}
	if c.FailRate > 0 && rand.Float64() < c.FailRate {
		s.Failures.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c.FailStatus)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"overloaded_error","message":"mock %s failure"}}`, c.Name)
		return c, false
	}
	return c, true
}

func estimateTokens(s string) int64 { return int64(len(s)+3) / 4 }

var filler = strings.Fields("the gateway forwards this request to the configured provider and returns the answer with usage and cost so that every call can be measured and capped")

func reply(c Config, model, prompt string) []string {
	head := fmt.Sprintf("[%s/%s] reply to %q:", c.Name, model, truncate(prompt, 40))
	words := strings.Fields(head)
	for i := 0; len(words) < c.OutputTokens; i++ {
		words = append(words, filler[i%len(filler)])
	}
	words = words[:c.OutputTokens]
	for i := 1; i < len(words); i++ {
		words[i] = " " + words[i]
	}
	return words
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func sse(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	if event != "" {
		fmt.Fprintf(w, "event: %s\n", event)
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func sleepCtx(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}

// ---- Anthropic Messages API ----

type anthReq struct {
	Model    string `json:"model"`
	System   any    `json:"system"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func (s *Server) anthropic(w http.ResponseWriter, r *http.Request) {
	var req anthReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages: at least one message is required"}}`)
		return
	}
	c, ok := s.begin(w, r, s.Config().Latency)
	if !ok {
		return
	}
	var promptText strings.Builder
	sysJSON, _ := json.Marshal(req.System)
	promptText.Write(sysJSON)
	var lastUser string
	for _, m := range req.Messages {
		text := blockText(m.Content)
		promptText.WriteString(text)
		if m.Role == "user" {
			lastUser = text
		}
	}
	inTok := estimateTokens(promptText.String())
	words := reply(c, req.Model, lastUser)
	id := fmt.Sprintf("msg_mock_%d", s.Requests.Load())
	refuse := c.RefuseRate > 0 && rand.Float64() < c.RefuseRate

	if !req.Stream {
		if !sleepCtx(r, time.Duration(len(words))*c.TokenDelay) {
			return
		}
		stop, content := "end_turn", []map[string]any{{"type": "text", "text": strings.Join(words, "")}}
		if refuse {
			stop, content = "refusal", []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model, "stop_reason": stop,
			"content": content, "usage": map[string]any{"input_tokens": inTok, "output_tokens": len(words)},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	sse(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "usage": map[string]any{"input_tokens": inTok, "output_tokens": 1},
	}})
	stop := "end_turn"
	if refuse {
		stop, words = "refusal", nil
	} else {
		sse(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		for _, word := range words {
			if !sleepCtx(r, c.TokenDelay) {
				return
			}
			sse(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": word}})
		}
		sse(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	}
	sse(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop},
		"usage": map[string]any{"output_tokens": len(words)}})
	sse(w, "message_stop", map[string]any{"type": "message_stop"})
}

// blockText reads message content that is either a string or a list of
// {"type":"text","text":...} blocks.
func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, " ")
}

// ---- OpenAI chat completions ----

type oaReq struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request) {
	var req oaReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 {
		http.Error(w, `{"error":{"message":"messages is required","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	c, ok := s.begin(w, r, s.Config().Latency)
	if !ok {
		return
	}
	var all strings.Builder
	last := ""
	for _, m := range req.Messages {
		all.WriteString(m.Content)
		if m.Role == "user" {
			last = m.Content
		}
	}
	inTok := estimateTokens(all.String())
	words := reply(c, req.Model, last)
	id := fmt.Sprintf("chatcmpl-mock-%d", s.Requests.Load())
	usage := map[string]any{"prompt_tokens": inTok, "completion_tokens": len(words), "total_tokens": inTok + int64(len(words))}

	if !req.Stream {
		if !sleepCtx(r, time.Duration(len(words))*c.TokenDelay) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "model": req.Model, "usage": usage,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": strings.Join(words, "")}}},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, word := range words {
		if !sleepCtx(r, c.TokenDelay) {
			return
		}
		sse(w, "", map[string]any{"id": id, "object": "chat.completion.chunk", "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": word}}}})
	}
	sse(w, "", map[string]any{"id": id, "object": "chat.completion.chunk", "model": req.Model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	sse(w, "", map[string]any{"id": id, "object": "chat.completion.chunk", "model": req.Model, "choices": []any{}, "usage": usage})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// ---- embeddings ----

func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var inputs []string
	switch v := req.Input.(type) {
	case string:
		inputs = []string{v}
	case []any:
		for _, x := range v {
			s, _ := x.(string)
			inputs = append(inputs, s)
		}
	}
	c, ok := s.begin(w, r, s.Config().EmbedLatency)
	if !ok {
		return
	}
	s.Embedded.Add(int64(len(inputs)))
	var tokens int64
	data := make([]map[string]any, len(inputs))
	for i, in := range inputs {
		tokens += estimateTokens(in)
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": Vector(in, c.EmbedDim)}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": req.Model, "data": data,
		"usage": map[string]any{"prompt_tokens": tokens, "total_tokens": tokens}})
}

// Vector is a deterministic unit vector derived from the text's hash.
func Vector(text string, dim int) []float32 {
	v := make([]float32, dim)
	seed := sha256.Sum256([]byte(text))
	var norm float64
	for i := range v {
		h := sha256.Sum256(append(seed[:], byte(i), byte(i>>8)))
		x := float64(int32(binary.LittleEndian.Uint32(h[:4]))) / math.MaxInt32
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
