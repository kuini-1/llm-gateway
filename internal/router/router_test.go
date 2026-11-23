package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuini-1/llm-gateway/internal/config"
	"github.com/kuini-1/llm-gateway/internal/provider"
)

// fake is a scripted Chatter: each call takes the next error from errs (nil = success).
type fake struct {
	name  string
	errs  []error
	calls int
	// streamFailAfter > 0 makes Stream send that many deltas, then fail.
	streamFailAfter int
}

func (f *fake) next() error {
	f.calls++
	if len(f.errs) == 0 {
		return nil
	}
	e := f.errs[0]
	if len(f.errs) > 1 {
		f.errs = f.errs[1:]
	}
	return e
}

func (f *fake) Chat(_ context.Context, r provider.ChatRequest) (*provider.ChatResponse, error) {
	if err := f.next(); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Content: f.name + ":" + r.Model}, nil
}

func (f *fake) Stream(ctx context.Context, r provider.ChatRequest, emit func(provider.Delta) error) (*provider.ChatResponse, error) {
	if f.streamFailAfter > 0 {
		for i := 0; i < f.streamFailAfter; i++ {
			emit(provider.Delta{Text: "x"})
		}
		return nil, provider.Classify(f.name, 502, "connection reset mid-stream", nil)
	}
	resp, err := f.Chat(ctx, r)
	if err == nil {
		emit(provider.Delta{Text: resp.Content})
	}
	return resp, err
}

func setup(threshold int, providers ...*fake) (*Router, config.Route) {
	cfg := &config.Config{Breaker: config.Breaker{FailureThreshold: threshold, OpenFor: time.Minute}, Routes: map[string]config.Route{}}
	route := config.Route{Kind: "chat"}
	for _, p := range providers {
		route.Targets = append(route.Targets, config.Target{Provider: p.name, Model: "m-" + p.name})
	}
	cfg.Routes["smart"] = route
	r := New(cfg)
	for _, p := range providers {
		r.SetProvider(p.name, p, nil)
	}
	return r, route
}

var req = provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: "hi"}}}

func status(code int) error { return provider.Classify("x", code, "boom", nil) }

func TestFallsBackOnProviderErrors(t *testing.T) {
	for _, code := range []int{503, 529, 429, 401, 404} {
		a, b := &fake{name: "a", errs: []error{status(code)}}, &fake{name: "b"}
		r, rt := setup(5, a, b)
		res, err := r.Chat(context.Background(), rt, req)
		if err != nil {
			t.Fatalf("%d: %v", code, err)
		}
		if res.Resp.Content != "b:m-b" || res.FallbackFrom() != "a/m-a" || len(res.Attempts) != 2 {
			t.Errorf("%d: result %+v", code, res)
		}
	}
}

func TestDoesNotFallBackOnInvalidRequest(t *testing.T) {
	a, b := &fake{name: "a", errs: []error{status(400)}}, &fake{name: "b"}
	r, rt := setup(5, a, b)
	_, err := r.Chat(context.Background(), rt, req)
	var all *AllFailed
	if !errors.As(err, &all) || b.calls != 0 {
		t.Fatalf("err = %v, b called %d times", err, b.calls)
	}
	if all.Last.Kind != provider.KindInvalid {
		t.Errorf("last kind = %s", all.Last.Kind)
	}
}

func TestRefusalFallbackIsOptIn(t *testing.T) {
	refusal := &provider.Error{Provider: "a", Kind: provider.KindRefused, Message: "declined"}
	a, b := &fake{name: "a", errs: []error{refusal}}, &fake{name: "b"}
	r, rt := setup(5, a, b)
	if _, err := r.Chat(context.Background(), rt, req); err == nil || b.calls != 0 {
		t.Fatal("refusal should not fall back by default")
	}
	a.errs = []error{refusal}
	rt.FallbackOnRefusal = true
	if res, err := r.Chat(context.Background(), rt, req); err != nil || res.Resp.Content != "b:m-b" {
		t.Fatalf("with fallback_on_refusal: %v", err)
	}
}

func TestBreakerOpensAndRecovers(t *testing.T) {
	a, b := &fake{name: "a", errs: []error{status(503)}}, &fake{name: "b"}
	r, rt := setup(3, a, b)
	for i := 0; i < 3; i++ {
		r.Chat(context.Background(), rt, req)
	}
	if a.calls != 3 {
		t.Fatalf("a called %d times before opening", a.calls)
	}
	// Open: a is skipped, b answers directly.
	res, err := r.Chat(context.Background(), rt, req)
	if err != nil || a.calls != 3 || !res.Attempts[0].Skipped {
		t.Fatalf("expected a to be skipped: %+v, %v", res.Attempts, err)
	}
	// After the cooldown one probe goes through; success closes the circuit.
	br := r.breakers["a/m-a"]
	br.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	a.errs = nil
	if res, _ := r.Chat(context.Background(), rt, req); res.Resp.Content != "a:m-a" {
		t.Fatalf("probe should go to a, got %q", res.Resp.Content)
	}
	if br.Status().State != Closed {
		t.Errorf("state = %s", br.Status().State)
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b := NewBreaker(1, time.Minute)
	now := time.Now()
	b.now = func() time.Time { return now }
	b.Failure("x")
	if b.Allow() {
		t.Fatal("should be open")
	}
	now = now.Add(2 * time.Minute)
	if !b.Allow() || b.Allow() {
		t.Fatal("half-open should allow exactly one probe")
	}
	b.Failure("still down")
	if b.Status().State != Open || b.Allow() {
		t.Fatal("failed probe should reopen the circuit")
	}
}

func TestAllOpenFailsFast(t *testing.T) {
	a := &fake{name: "a", errs: []error{status(503)}}
	r, rt := setup(1, a)
	r.Chat(context.Background(), rt, req) // opens
	a.errs = nil
	_, err := r.Chat(context.Background(), rt, req)
	var all *AllFailed
	if !errors.As(err, &all) || all.Last != ErrCircuitOpen || a.calls != 1 {
		t.Fatalf("expected a fast failure without calling a, got %v (calls %d)", err, a.calls)
	}
}

func TestSkippedPrimaryCountsAsFallback(t *testing.T) {
	a, b := &fake{name: "a", errs: []error{status(503)}}, &fake{name: "b"}
	r, rt := setup(1, a, b)
	r.Chat(context.Background(), rt, req) // a fails once: circuit opens
	res, err := r.Chat(context.Background(), rt, req)
	if err != nil || !res.Attempts[0].Skipped || res.FallbackFrom() != "a/m-a" {
		t.Fatalf("skipped primary should still count as a fallback: %+v %v", res, err)
	}
}

func TestStreamDoesNotFallBackAfterFirstToken(t *testing.T) {
	a, b := &fake{name: "a", streamFailAfter: 2}, &fake{name: "b"}
	r, rt := setup(5, a, b)
	var got strings.Builder
	_, err := r.Stream(context.Background(), rt, req, func(d provider.Delta) error { got.WriteString(d.Text); return nil })
	if err == nil || b.calls != 0 || got.String() != "xx" {
		t.Fatalf("err %v, b calls %d, streamed %q", err, b.calls, got.String())
	}
}

func TestStreamFallsBackBeforeFirstToken(t *testing.T) {
	a, b := &fake{name: "a", errs: []error{status(503)}}, &fake{name: "b"}
	r, rt := setup(5, a, b)
	var got strings.Builder
	res, err := r.Stream(context.Background(), rt, req, func(d provider.Delta) error { got.WriteString(d.Text); return nil })
	if err != nil || got.String() != "b:m-b" || res.TTFB == 0 && got.Len() == 0 {
		t.Fatalf("err %v, streamed %q", err, got.String())
	}
}

func TestResolveDirectAddress(t *testing.T) {
	r, _ := setup(5, &fake{name: "a"})
	rt, err := r.Resolve("a/some-model", "chat")
	if err != nil || len(rt.Targets) != 1 || rt.Targets[0].Model != "some-model" {
		t.Fatalf("resolve: %+v %v", rt, err)
	}
	if _, err := r.Resolve("nope", "chat"); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("unknown model: %v", err)
	}
	if _, err := r.Resolve("smart", "embedding"); err == nil {
		t.Error("a chat route must not resolve for embeddings")
	}
}
