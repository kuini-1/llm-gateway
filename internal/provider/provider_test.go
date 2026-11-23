package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuini-1/llm-gateway/internal/mockllm"
)

func mock(t *testing.T, cfg mockllm.Config) (*mockllm.Server, string) {
	t.Helper()
	m := mockllm.New(cfg)
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	return m, srv.URL
}

var req = ChatRequest{
	Model:     "claude-opus-5-5",
	MaxTokens: 100,
	Messages: []Message{
		{Role: "system", Content: "Be brief."},
		{Role: "user", Content: "Hello"},
		{Role: "user", Content: "Second user turn in a row"},
	},
}

func TestAnthropicChatAndStream(t *testing.T) {
	_, url := mock(t, mockllm.Config{Name: "anthropic", OutputTokens: 12})
	a := NewAnthropic("anthropic", url, "test-key", 5*time.Second, true, true)

	resp, err := a.Chat(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "Second user turn") || resp.Usage.OutputTokens != 12 || resp.FinishReason != "stop" {
		t.Errorf("chat = %+v", resp)
	}

	var streamed strings.Builder
	sresp, err := a.Stream(context.Background(), req, func(d Delta) error { streamed.WriteString(d.Text); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != sresp.Content || sresp.Usage.OutputTokens != 12 || sresp.Usage.InputTokens == 0 {
		t.Errorf("stream: deltas %q, final %+v", streamed.String(), sresp)
	}
}

func TestAnthropicDropsTemperature(t *testing.T) {
	_, url := mock(t, mockllm.Config{Name: "anthropic"})
	a := NewAnthropic("anthropic", url, "k", 5*time.Second, false, false)
	temp := 0.2
	r := req
	r.Temperature = &temp
	resp, err := a.Chat(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "temperature") {
		t.Errorf("warnings = %v", resp.Warnings)
	}
}

func TestAnthropicRefusal(t *testing.T) {
	_, url := mock(t, mockllm.Config{Name: "anthropic", RefuseRate: 1})
	a := NewAnthropic("anthropic", url, "k", 5*time.Second, false, false)
	_, err := a.Chat(context.Background(), req)
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindRefused {
		t.Fatalf("err = %v, want refused", err)
	}
	_, err = a.Stream(context.Background(), req, func(Delta) error { return nil })
	if !errors.As(err, &pe) || pe.Kind != KindRefused {
		t.Fatalf("stream err = %v, want refused", err)
	}
}

func TestOpenAICompat(t *testing.T) {
	_, url := mock(t, mockllm.Config{Name: "openai", OutputTokens: 8, EmbedDim: 16})
	o := NewOpenAICompat("openai", url+"/v1", "k", 5*time.Second)
	r := req
	r.Model = "gpt-test"
	resp, err := o.Chat(context.Background(), r)
	if err != nil || resp.Usage.OutputTokens != 8 {
		t.Fatalf("chat = %+v, %v", resp, err)
	}
	var n int
	sresp, err := o.Stream(context.Background(), r, func(Delta) error { n++; return nil })
	if err != nil || n != 8 || sresp.Usage.OutputTokens != 8 {
		t.Fatalf("stream: %d deltas, %+v, %v", n, sresp, err)
	}
	emb, err := o.Embed(context.Background(), EmbedRequest{Model: "emb", Input: []string{"a", "b"}})
	if err != nil || len(emb.Vectors) != 2 || len(emb.Vectors[0]) != 16 {
		t.Fatalf("embed = %+v, %v", emb, err)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		status    int
		kind      ErrorKind
		retryable bool
	}{
		{http.StatusServiceUnavailable, KindUnavailable, true},
		{529, KindUnavailable, true},
		{http.StatusTooManyRequests, KindRateLimited, true},
		{http.StatusUnauthorized, KindAuth, true},
		{http.StatusNotFound, KindUnavailable, true},
		{http.StatusBadRequest, KindInvalid, false},
		{http.StatusRequestEntityTooLarge, KindInvalid, false},
	}
	for _, c := range cases {
		e := Classify("p", c.status, "", nil)
		if e.Kind != c.kind || e.Retryable() != c.retryable {
			t.Errorf("status %d: kind %s retryable %v", c.status, e.Kind, e.Retryable())
		}
	}
	if e := Classify("p", 0, "", context.DeadlineExceeded); e.Kind != KindTimeout || !e.Retryable() {
		t.Errorf("timeout classified as %s", e.Kind)
	}
}

func TestUpstreamFailureIsClassified(t *testing.T) {
	_, url := mock(t, mockllm.Config{Name: "anthropic", FailRate: 1, FailStatus: 529})
	a := NewAnthropic("anthropic", url, "k", 5*time.Second, false, false)
	_, err := a.Chat(context.Background(), req)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 529 || !pe.Retryable() {
		t.Fatalf("err = %v", err)
	}
}
