// Package provider talks to upstream model APIs behind one interface. The
// gateway's own request and response shapes follow the OpenAI chat format,
// which most client libraries speak; each adapter translates to its API.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

type Message struct {
	Role    string `json:"role"` // system, user, assistant
	Content string `json:"content"`
}

type ChatRequest struct {
	Model           string    // the upstream model id (set by the router)
	Messages        []Message //
	MaxTokens       int
	Temperature     *float64
	ReasoningEffort string // low, medium, high (mapped per provider)
	User            string
}

type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type ChatResponse struct {
	ID           string
	Model        string // as reported by the provider
	Content      string
	FinishReason string // stop, length, content_filter, tool_calls
	Usage        Usage
	Warnings     []string // parameters the provider ignored, etc.
}

// Delta is one streamed piece of text.
type Delta struct {
	Text string
}

type EmbedRequest struct {
	Model string
	Input []string
}

type EmbedResponse struct {
	Vectors [][]float32
	Tokens  int64
}

type Chatter interface {
	Chat(ctx context.Context, r ChatRequest) (*ChatResponse, error)
	// Stream calls emit for every text delta and returns the final response.
	Stream(ctx context.Context, r ChatRequest, emit func(Delta) error) (*ChatResponse, error)
}

type Embedder interface {
	Embed(ctx context.Context, r EmbedRequest) (*EmbedResponse, error)
}

// Error is an upstream failure with enough detail to decide whether another
// provider might succeed.
type Error struct {
	Provider string
	Status   int // HTTP status, 0 for transport errors
	Message  string
	Kind     ErrorKind
	Err      error
}

type ErrorKind string

const (
	// Retryable: the provider is unavailable, overloaded or misconfigured;
	// another target may well succeed.
	KindUnavailable ErrorKind = "unavailable"
	KindRateLimited ErrorKind = "rate_limited"
	KindTimeout     ErrorKind = "timeout"
	KindAuth        ErrorKind = "auth" // our key for this provider is wrong: other providers are unaffected
	KindRefused     ErrorKind = "refused"
	// Not retryable: the request itself is invalid and would fail anywhere.
	KindInvalid ErrorKind = "invalid_request"
)

func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("%s: HTTP %d (%s): %s", e.Provider, e.Status, e.Kind, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Provider, e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether trying the next target makes sense.
func (e *Error) Retryable() bool { return e.Kind != KindInvalid }

// Classify maps an HTTP status or transport error to an ErrorKind.
func Classify(provider string, status int, msg string, err error) *Error {
	e := &Error{Provider: provider, Status: status, Message: msg, Err: err}
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		e.Kind = KindTimeout
	case err != nil:
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			e.Kind = KindTimeout
		} else {
			e.Kind = KindUnavailable
		}
		if e.Message == "" {
			e.Message = err.Error()
		}
	case status == http.StatusTooManyRequests:
		e.Kind = KindRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind = KindAuth
	case status == http.StatusNotFound:
		// Usually a model this provider doesn't serve: a config problem, not the client's.
		e.Kind = KindUnavailable
	case status == http.StatusRequestTimeout:
		e.Kind = KindTimeout
	case status >= 500:
		e.Kind = KindUnavailable // includes Anthropic's 529 "overloaded"
	case status >= 400:
		e.Kind = KindInvalid
	default:
		e.Kind = KindUnavailable
	}
	return e
}

// AsError extracts a *Error, wrapping anything else as unavailable.
func AsError(provider string, err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Classify(provider, 0, "", err)
}
