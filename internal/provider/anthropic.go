package provider

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// Anthropic calls the Messages API through the official SDK.
type Anthropic struct {
	Name               string
	client             anthropic.Client
	serverSideFallback bool
	promptCaching      bool
}

func NewAnthropic(name, baseURL, apiKey string, timeout time.Duration, serverSideFallback, promptCaching bool) *Anthropic {
	opts := []option.RequestOption{
		option.WithMaxRetries(0), // the gateway decides what to retry and where
		option.WithRequestTimeout(timeout),
	}
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Anthropic{Name: name, client: anthropic.NewClient(opts...), serverSideFallback: serverSideFallback, promptCaching: promptCaching}
}

// Models that accept server-side refusal fallbacks ("default" routing).
var fallbackModels = map[string]bool{
	"claude-fable-5-2": true, "claude-fable-5-1": true, "claude-opus-5-5": true, "claude-opus-5": true, "claude-sonnet-5-5": true,
}

func (a *Anthropic) params(r ChatRequest) (anthropic.BetaMessageNewParams, []string) {
	var warnings []string
	p := anthropic.BetaMessageNewParams{Model: anthropic.Model(r.Model), MaxTokens: int64(r.MaxTokens)}

	// System messages go in the top-level system field; the rest must
	// alternate user/assistant, so consecutive same-role turns are merged.
	var system []string
	for _, m := range r.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Content)
		case "assistant":
			p.Messages = appendTurn(p.Messages, true, m.Content)
		default:
			p.Messages = appendTurn(p.Messages, false, m.Content)
		}
	}
	if len(system) > 0 {
		p.System = []anthropic.BetaTextBlockParam{{Text: strings.Join(system, "\n\n")}}
	}
	if r.Temperature != nil {
		// Current Claude models reject sampling parameters; dropping is safer than failing.
		warnings = append(warnings, "temperature is not supported by Claude models and was ignored")
	}
	switch r.ReasoningEffort {
	case "low", "medium", "high", "xhigh", "max":
		p.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(r.ReasoningEffort)}
	case "":
	default:
		warnings = append(warnings, "unknown reasoning_effort "+r.ReasoningEffort+" was ignored")
	}
	if r.User != "" {
		p.Metadata = anthropic.BetaMetadataParam{UserID: anthropic.String(r.User)}
	}
	if a.promptCaching {
		p.CacheControl = anthropic.NewBetaCacheControlEphemeralParam()
	}
	if a.serverSideFallback && fallbackModels[r.Model] {
		p.Fallbacks = anthropic.BetaFallbacksParamOfDefault()
		p.Betas = append(p.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}
	return p, warnings
}

func appendTurn(msgs []anthropic.BetaMessageParam, assistant bool, text string) []anthropic.BetaMessageParam {
	if n := len(msgs); n > 0 {
		last := msgs[n-1]
		if assistant == (last.OfAssistant != nil) {
			// Same role twice in a row: add another text block to that turn.
			if assistant {
				last.OfAssistant.Content = append(last.OfAssistant.Content, anthropic.NewBetaTextBlock(text))
			} else {
				last.OfUser.Content = append(last.OfUser.Content, anthropic.NewBetaTextBlock(text))
			}
			return msgs
		}
	}
	if assistant {
		return append(msgs, anthropic.BetaMessageParam{OfAssistant: &anthropic.BetaAssistantMessageParam{
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(text)}}})
	}
	return append(msgs, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
}

func (a *Anthropic) Chat(ctx context.Context, r ChatRequest) (*ChatResponse, error) {
	p, warnings := a.params(r)
	msg, err := a.client.Beta.Messages.New(ctx, p)
	if err != nil {
		return nil, a.err(err)
	}
	return a.response(msg, warnings)
}

func (a *Anthropic) Stream(ctx context.Context, r ChatRequest, emit func(Delta) error) (*ChatResponse, error) {
	p, warnings := a.params(r)
	stream := a.client.Beta.Messages.NewStreaming(ctx, p)
	defer stream.Close()
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return nil, &Error{Provider: a.Name, Kind: KindUnavailable, Message: "bad stream: " + err.Error(), Err: err}
		}
		if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(anthropic.BetaTextDelta); ok && t.Text != "" {
				if err := emit(Delta{Text: t.Text}); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return nil, a.err(err)
	}
	return a.response(&msg, warnings)
}

func (a *Anthropic) response(m *anthropic.BetaMessage, warnings []string) (*ChatResponse, error) {
	if m.StopReason == anthropic.BetaStopReasonRefusal {
		msg := "the model declined the request"
		if c := string(m.StopDetails.Category); c != "" {
			msg += " (" + c + ")"
		}
		return nil, &Error{Provider: a.Name, Kind: KindRefused, Message: msg}
	}
	var sb strings.Builder
	for _, c := range m.Content {
		if t, ok := c.AsAny().(anthropic.BetaTextBlock); ok {
			sb.WriteString(t.Text)
		}
	}
	return &ChatResponse{
		ID: m.ID, Model: string(m.Model), Content: sb.String(), FinishReason: finishReason(m.StopReason),
		Usage: Usage{
			InputTokens: m.Usage.InputTokens, OutputTokens: m.Usage.OutputTokens,
			CacheReadTokens: m.Usage.CacheReadInputTokens, CacheWriteTokens: m.Usage.CacheCreationInputTokens,
		},
		Warnings: warnings,
	}, nil
}

func finishReason(s anthropic.BetaStopReason) string {
	switch s {
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		return "length"
	case anthropic.BetaStopReasonToolUse:
		return "tool_calls"
	default:
		return "stop"
	}
}

func (a *Anthropic) err(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return Classify(a.Name, apiErr.StatusCode, apiErr.Error(), nil)
	}
	return Classify(a.Name, 0, "", err)
}
