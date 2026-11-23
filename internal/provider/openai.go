package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompat speaks the OpenAI chat completions and embeddings format,
// which OpenAI, DeepSeek and most self-hosted servers share.
type OpenAICompat struct {
	Name    string
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func NewOpenAICompat(name, baseURL, apiKey string, timeout time.Duration) *OpenAICompat {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAICompat{Name: name, BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: timeout}}
}

type oaChatReq struct {
	Model           string         `json:"model"`
	Messages        []Message      `json:"messages"`
	MaxTokens       int            `json:"max_completion_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	User            string         `json:"user,omitempty"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   map[string]any `json:"stream_options,omitempty"`
}

type oaUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// DeepSeek reports cache hits this way.
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
}

func (u oaUsage) toUsage() Usage {
	cached := max(u.PromptTokensDetails.CachedTokens, u.PromptCacheHitTokens)
	return Usage{InputTokens: u.PromptTokens - cached, OutputTokens: u.CompletionTokens, CacheReadTokens: cached}
}

type oaChatResp struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *oaUsage `json:"usage"`
}

func (o *OpenAICompat) post(ctx context.Context, path string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, Classify(o.Name, 0, "", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, Classify(o.Name, resp.StatusCode, strings.TrimSpace(string(msg)), nil)
	}
	return resp, nil
}

func (o *OpenAICompat) chatBody(r ChatRequest, stream bool) oaChatReq {
	b := oaChatReq{Model: r.Model, Messages: r.Messages, MaxTokens: r.MaxTokens, Temperature: r.Temperature,
		ReasoningEffort: r.ReasoningEffort, User: r.User, Stream: stream}
	if stream {
		b.StreamOptions = map[string]any{"include_usage": true}
	}
	return b
}

func (o *OpenAICompat) Chat(ctx context.Context, r ChatRequest) (*ChatResponse, error) {
	resp, err := o.post(ctx, "/chat/completions", o.chatBody(r, false))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out oaChatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Classify(o.Name, 0, "invalid response JSON", err)
	}
	if len(out.Choices) == 0 {
		return nil, &Error{Provider: o.Name, Kind: KindUnavailable, Message: "response has no choices"}
	}
	cr := &ChatResponse{ID: out.ID, Model: out.Model, Content: out.Choices[0].Message.Content, FinishReason: out.Choices[0].FinishReason}
	if out.Usage != nil {
		cr.Usage = out.Usage.toUsage()
	}
	if cr.FinishReason == "content_filter" {
		return nil, &Error{Provider: o.Name, Kind: KindRefused, Message: "the provider's content filter stopped the response"}
	}
	return cr, nil
}

func (o *OpenAICompat) Stream(ctx context.Context, r ChatRequest, emit func(Delta) error) (*ChatResponse, error) {
	resp, err := o.post(ctx, "/chat/completions", o.chatBody(r, true))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	cr := &ChatResponse{}
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk oaChatResp
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, &Error{Provider: o.Name, Kind: KindUnavailable, Message: "bad stream chunk", Err: err}
		}
		if cr.ID == "" {
			cr.ID, cr.Model = chunk.ID, chunk.Model
		}
		if chunk.Usage != nil {
			cr.Usage = chunk.Usage.toUsage()
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				sb.WriteString(ch.Delta.Content)
				if err := emit(Delta{Text: ch.Delta.Content}); err != nil {
					return nil, err
				}
			}
			if ch.FinishReason != "" {
				cr.FinishReason = ch.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, Classify(o.Name, 0, "stream interrupted", err)
	}
	cr.Content = sb.String()
	return cr, nil
}

func (o *OpenAICompat) Embed(ctx context.Context, r EmbedRequest) (*EmbedResponse, error) {
	resp, err := o.post(ctx, "/embeddings", map[string]any{"model": r.Model, "input": r.Input})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, Classify(o.Name, 0, "invalid response JSON", err)
	}
	if len(out.Data) != len(r.Input) {
		return nil, &Error{Provider: o.Name, Kind: KindUnavailable, Message: fmt.Sprintf("got %d embeddings for %d inputs", len(out.Data), len(r.Input))}
	}
	vecs := make([][]float32, len(r.Input))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			return nil, &Error{Provider: o.Name, Kind: KindUnavailable, Message: "embedding index out of range"}
		}
		vecs[d.Index] = d.Embedding
	}
	return &EmbedResponse{Vectors: vecs, Tokens: out.Usage.PromptTokens}, nil
}
