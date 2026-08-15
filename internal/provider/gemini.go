package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Gemini adapts Google's generateContent API.
//
// It differs from the OpenAI dialect in nearly every dimension, which is why it
// is a useful stand-in for the Anthropic slot: messages are "contents" holding
// "parts", the assistant role is called "model", generation settings live in a
// nested object, the model name goes in the URL path rather than the body, and
// usage comes back under different field names. If the canonical types survive
// this adapter unchanged, they are genuinely provider-neutral.
type Gemini struct {
	base
}

var _ Provider = (*Gemini)(nil)

func NewGemini(cfg Config) (*Gemini, error) {
	b, err := newBase(cfg)
	if err != nil {
		return nil, err
	}
	return &Gemini{base: b}, nil
}

func (p *Gemini) Stream(context.Context, Request) (StreamReader, error) {
	return nil, ErrStreamingNotImplemented
}

func (p *Gemini) Ping(ctx context.Context) error {
	model, err := p.pingModel()
	if err != nil {
		return err
	}
	_, err = p.Complete(ctx, pingRequest(model))
	return err
}

func (p *Gemini) Complete(ctx context.Context, req Request) (*Response, error) {
	// The model is part of the path here, not the body. PathEscape guards
	// against a config entry with a character that would change the URL's
	// shape.
	endpoint := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/models/" + url.PathEscape(req.Model) + ":generateContent"

	headers := map[string]string{}
	if p.cfg.APIKey != "" {
		// Google also accepts ?key= in the query string. The header is used
		// instead so the credential never lands in a URL, which would put it in
		// access logs and in the Phase 8 trace attributes.
		headers["x-goog-api-key"] = p.cfg.APIKey
	}

	res, err := postJSON(ctx, p.cfg, p.client, endpoint, req.Model, headers, p.translateRequest(req))
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

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiGenerationConfig struct {
	Temperature     *float32 `json:"temperature,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type geminiRequest struct {
	Contents          []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

type geminiErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// --- translation ------------------------------------------------------------

func (p *Gemini) translateRequest(req Request) geminiRequest {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}

	var systemParts []string
	contents := make([]geminiContent, 0, len(req.Messages))

	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			systemParts = append(systemParts, m.Content)
			continue
		}
		contents = append(contents, geminiContent{
			Role:  geminiRole(m.Role),
			Parts: []geminiPart{{Text: m.Content}},
		})
	}

	out := geminiRequest{
		Contents: contents,
		GenerationConfig: geminiGenerationConfig{
			Temperature:     req.Temperature,
			MaxOutputTokens: maxTokens,
			StopSequences:   req.Stop,
		},
	}

	if len(systemParts) > 0 {
		out.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: strings.Join(systemParts, "\n\n")}},
		}
	}

	return out
}

// geminiRole maps the canonical roles onto Gemini's vocabulary, where the
// assistant is called "model".
func geminiRole(r Role) string {
	if r == RoleAssistant {
		return "model"
	}
	return string(r)
}

func (p *Gemini) translateResponse(model string, raw []byte, latency time.Duration) (*Response, error) {
	var out geminiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, malformedResponse(p.cfg.Name, model, "upstream returned a body that is not a valid generateContent response", err)
	}

	// The interesting case, and the reason KindContentPolicy cannot be derived
	// from a status code: Gemini answers HTTP 200 with no candidates and a
	// blockReason when it refuses a prompt. A status-only classifier would read
	// this as success and hand back an empty completion.
	if out.PromptFeedback.BlockReason != "" {
		return nil, &Error{
			Kind:       KindContentPolicy,
			Provider:   p.cfg.Name,
			Model:      model,
			StatusCode: http.StatusOK,
			Retryable:  false,
			Message:    "prompt blocked: " + truncateMessage(out.PromptFeedback.BlockReason),
		}
	}

	if len(out.Candidates) == 0 {
		return nil, malformedResponse(p.cfg.Name, model, "upstream returned no candidates", nil)
	}

	candidate := out.Candidates[0]

	var content strings.Builder
	for _, part := range candidate.Content.Parts {
		content.WriteString(part.Text)
	}

	served := out.ModelVersion
	if served == "" {
		served = model
	}

	return &Response{
		Content:      content.String(),
		FinishReason: geminiFinishReason(candidate.FinishReason),
		Usage: Usage{
			InputTokens:  out.UsageMetadata.PromptTokenCount,
			OutputTokens: out.UsageMetadata.CandidatesTokenCount,
		},
		Model:    served,
		Provider: p.cfg.Name,
		Latency:  latency,
	}, nil
}

func geminiFinishReason(s string) FinishReason {
	switch s {
	case "STOP":
		return FinishStop
	case "MAX_TOKENS":
		return FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return FinishContentFilter
	default:
		return FinishOther
	}
}

func (p *Gemini) classify(model string, status int, body []byte) *Error {
	var env geminiErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		env = geminiErrorEnvelope{}
	}

	message := truncateMessage(env.Error.Message)
	if message == "" {
		message = truncateMessage(string(body))
	}

	provErr := NewHTTPError(p.cfg.Name, model, status, message)

	// Google reports a canonical status string alongside the HTTP code, and the
	// two do not always agree in usefulness. Trust the string where it is more
	// specific.
	switch env.Error.Status {
	case "RESOURCE_EXHAUSTED":
		provErr.Kind = KindRateLimited
		provErr.Retryable = true
	case "UNAUTHENTICATED", "PERMISSION_DENIED":
		provErr.Kind = KindAuthFailed
		provErr.Retryable = false
	case "INVALID_ARGUMENT", "FAILED_PRECONDITION", "NOT_FOUND":
		// FAILED_PRECONDITION is how Gemini reports "free tier unavailable in
		// your region" — permanent, and retrying it wastes the fallback chain.
		provErr.Kind = KindInvalidRequest
		provErr.Retryable = false
	case "UNAVAILABLE", "INTERNAL", "DEADLINE_EXCEEDED":
		provErr.Kind = KindServerError
		provErr.Retryable = true
	}

	return provErr
}
