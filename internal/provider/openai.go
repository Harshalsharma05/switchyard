package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// OpenAICompatible adapts any provider speaking OpenAI's /v1/chat/completions
// dialect. Real OpenAI and Groq are both served by this one implementation —
// they differ only in base URL, credential, and model list, all of which are
// config. That is the entire reason Groq can stand in for OpenAI during
// development without a line of Go changing.
type OpenAICompatible struct {
	base
}

// Compile-time proof that this satisfies Provider. Go matches interfaces
// implicitly, so without this line a missing method would surface as a
// confusing error wherever the value is used, rather than in this file.
var _ Provider = (*OpenAICompatible)(nil)

// NewOpenAICompatible validates config and builds the adapter. It returns an
// error rather than panicking so Step 1.4's registry can refuse to start on a
// malformed config with a message naming the offending instance.
func NewOpenAICompatible(cfg Config) (*OpenAICompatible, error) {
	b, err := newBase(cfg)
	if err != nil {
		return nil, err
	}
	return &OpenAICompatible{base: b}, nil
}

// Stream is Phase 2 work. The Step 1.5 handler rejects stream:true before a
// request reaches here; this is the backstop.
func (p *OpenAICompatible) Stream(context.Context, Request) (StreamReader, error) {
	return nil, ErrStreamingNotImplemented
}

// Ping issues the cheapest possible completion for the Phase 5 health checker.
// The checker supplies its own bounded context, so no separate timeout is
// needed here.
func (p *OpenAICompatible) Ping(ctx context.Context) error {
	model, err := p.pingModel()
	if err != nil {
		return err
	}
	_, err = p.Complete(ctx, pingRequest(model))
	return err
}

func (p *OpenAICompatible) Complete(ctx context.Context, req Request) (*Response, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/chat/completions"

	headers := map[string]string{}
	if p.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + p.cfg.APIKey
	}

	res, err := postJSON(ctx, p.cfg, p.client, url, req.Model, headers, p.translateRequest(req))
	if err != nil {
		return nil, err
	}

	if res.Status >= http.StatusBadRequest {
		provErr := p.classify(req.Model, res.Status, res.Body)
		provErr.RetryAfter = parseRetryAfter(res.Header.Get("Retry-After"), time.Now())
		return nil, provErr
	}

	return p.translateResponse(req.Model, res.Body, res.Latency)
}

// --- wire types -------------------------------------------------------------
//
// These mirror OpenAI's schema and exist only inside this file. They are
// separate from provider.Request/Response on purpose: the moment the canonical
// type doubles as a wire type, OpenAI's schema has become SwitchYard's internal
// format and every other adapter inherits its shape.

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiChatRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float32     `json:"temperature,omitempty"`
	Stop        []string     `json:"stop,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
}

type oaiChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type oaiErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// --- translation ------------------------------------------------------------

func (p *OpenAICompatible) translateRequest(req Request) oaiChatRequest {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}

	// System messages stay in the array here — this dialect wants them inline,
	// which is why the canonical Request keeps them as messages and lets the
	// adapters that need them hoisted do the hoisting.
	msgs := make([]oaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, oaiMessage{Role: string(m.Role), Content: m.Content})
	}

	return oaiChatRequest{
		Model:       req.Model,
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Stop:        req.Stop,
	}
}

func (p *OpenAICompatible) translateResponse(model string, raw []byte, latency time.Duration) (*Response, error) {
	var out oaiChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, malformedResponse(p.cfg.Name, model, "upstream returned a body that is not valid chat-completion JSON", err)
	}
	if len(out.Choices) == 0 {
		return nil, malformedResponse(p.cfg.Name, model, "upstream returned no choices", nil)
	}

	served := out.Model
	if served == "" {
		served = model
	}

	return &Response{
		Content:      out.Choices[0].Message.Content,
		FinishReason: oaiFinishReason(out.Choices[0].FinishReason),
		Usage: Usage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
		},
		Model:    served,
		Provider: p.cfg.Name,
		Latency:  latency,
	}, nil
}

func oaiFinishReason(s string) FinishReason {
	switch s {
	case "stop":
		return FinishStop
	case "length":
		return FinishLength
	case "content_filter":
		return FinishContentFilter
	default:
		return FinishOther
	}
}

// classify turns an error response into a provider.Error, starting from the
// status code and then refining using the body.
//
// The refinement is the point. A 429 is retryable when it is throttling and
// permanently hopeless when the account's credit is gone, and only the body
// distinguishes them. This is exactly why Error.Retryable is a stored field
// rather than something the retry layer derives from Kind.
func (p *OpenAICompatible) classify(model string, status int, body []byte) *Error {
	var env oaiErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		// Not silently swallowed: an upstream proxy can answer a 502 with HTML,
		// and there is nothing to learn from that. Keep the status-derived
		// classification and move on.
		env = oaiErrorEnvelope{}
	}

	message := truncateMessage(env.Error.Message)
	if message == "" {
		message = truncateMessage(string(body))
	}

	provErr := NewHTTPError(p.cfg.Name, model, status, message)

	switch {
	case env.Error.Code == "content_filter", env.Error.Type == "content_filter":
		provErr.Kind = KindContentPolicy
		provErr.Retryable = false

	case env.Error.Type == "insufficient_quota":
		// A 429 that will never clear on its own: billing is exhausted, not
		// throttled. Retrying burns the whole fallback chain for nothing.
		provErr.Retryable = false
	}

	return provErr
}
