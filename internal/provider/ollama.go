package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Ollama adapts a local Ollama server's /api/chat endpoint.
//
// It takes no credential, which is why validateConfig does not require an API
// key. As the free local fallback it is the last link in the Phase 6 chain, so
// its error classification matters as much as the paid providers'.
type Ollama struct {
	base
}

var _ Provider = (*Ollama)(nil)

func NewOllama(cfg Config) (*Ollama, error) {
	b, err := newBase(cfg)
	if err != nil {
		return nil, err
	}
	return &Ollama{base: b}, nil
}

// Stream hits the same /api/chat endpoint as Complete with stream:true, which
// switches Ollama's response from one JSON object to newline-delimited JSON —
// a different wire shape from OpenAI and Anthropic's SSE, handled by
// ndjsonStreamReader instead of sseStreamReader.
func (p *Ollama) Stream(ctx context.Context, req Request) (StreamReader, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/api/chat"

	resp, err := openStream(ctx, p.cfg, p.client, url, req.Model, nil, p.translateRequest(req, true))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, readStreamError(resp, p.cfg, req.Model, p.classify)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), maxStreamLineBytes)

	return &ndjsonStreamReader{
		body:    resp.Body,
		scanner: scanner,
		decode:  p.decodeStreamLine,
	}, nil
}

func (p *Ollama) Ping(ctx context.Context) error {
	model, err := p.pingModel()
	if err != nil {
		return err
	}
	_, err = p.Complete(ctx, pingRequest(model))
	return err
}

func (p *Ollama) Complete(ctx context.Context, req Request) (*Response, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/api/chat"

	// No auth header: Ollama runs locally and takes no key.
	res, err := postJSON(ctx, p.cfg, p.client, url, req.Model, nil, p.translateRequest(req, false))
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

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	Temperature *float32 `json:"temperature,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`

	// Stream has no omitempty, and that is load-bearing: Ollama streams by
	// default, so omitting the field gives back newline-delimited JSON that
	// this non-streaming path cannot parse. It must be sent as an explicit
	// false.
	Stream bool `json:"stream"`

	Options ollamaOptions `json:"options,omitempty"`
}

type ollamaResponse struct {
	Model      string        `json:"model"`
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason"`

	// Ollama names its token counts after the evaluation phases rather than
	// input and output.
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// ollamaErrorEnvelope is a bare string, not a nested object like every other
// provider here.
type ollamaErrorEnvelope struct {
	Error string `json:"error"`
}

// --- translation ------------------------------------------------------------

func (p *Ollama) translateRequest(req Request, stream bool) ollamaRequest {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}

	// System messages stay inline: Ollama handles the system role natively.
	msgs := make([]ollamaMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, ollamaMessage{Role: string(m.Role), Content: m.Content})
	}

	return ollamaRequest{
		Model:    req.Model,
		Messages: msgs,
		Stream:   stream,
		Options: ollamaOptions{
			Temperature: req.Temperature,
			NumPredict:  maxTokens,
			Stop:        req.Stop,
		},
	}
}

func (p *Ollama) translateResponse(model string, raw []byte, latency time.Duration) (*Response, error) {
	var out ollamaResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, malformedResponse(p.cfg.Name, model, "upstream returned a body that is not a valid chat response", err)
	}

	served := out.Model
	if served == "" {
		served = model
	}

	return &Response{
		Content:      out.Message.Content,
		FinishReason: ollamaFinishReason(out.DoneReason),
		Usage: Usage{
			InputTokens:  out.PromptEvalCount,
			OutputTokens: out.EvalCount,
		},
		Model:    served,
		Provider: p.cfg.Name,
		Latency:  latency,
	}, nil
}

func ollamaFinishReason(s string) FinishReason {
	switch s {
	case "stop":
		return FinishStop
	case "length":
		return FinishLength
	default:
		return FinishOther
	}
}

// decodeStreamLine turns one NDJSON line into a Chunk. It satisfies
// ndjsonStreamReader's decode field.
//
// It never reports done=true itself: the final line (done:true) still carries
// content and usage worth forwarding, and the stream simply ends after it —
// ndjsonStreamReader's Recv reaches that end the same way it reaches the end
// of any other line-delimited body, via scanner.Scan() returning false.
func (p *Ollama) decodeStreamLine(line []byte) (*Chunk, bool, error) {
	var wire ollamaResponse
	if err := json.Unmarshal(line, &wire); err != nil {
		return nil, false, fmt.Errorf("decoding %s stream line: %w", p.cfg.Name, err)
	}

	chunk := &Chunk{Content: wire.Message.Content}
	if wire.Done {
		chunk.FinishReason = ollamaFinishReason(wire.DoneReason)
		chunk.Usage = &Usage{InputTokens: wire.PromptEvalCount, OutputTokens: wire.EvalCount}
	}
	return chunk, false, nil
}

func (p *Ollama) classify(model string, status int, body []byte) *Error {
	var env ollamaErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		env = ollamaErrorEnvelope{}
	}

	message := truncateMessage(env.Error)
	if message == "" {
		message = truncateMessage(string(body))
	}

	provErr := NewHTTPError(p.cfg.Name, model, status, message)

	// Ollama reports a model that was never pulled as a 404. Falling back to
	// another provider is the right move; retrying this one is not, and the
	// status-derived KindInvalidRequest already says so. Asserted here because
	// it is the failure a fresh clone hits first.
	if status == http.StatusNotFound && strings.Contains(strings.ToLower(env.Error), "not found") {
		provErr.Kind = KindInvalidRequest
		provErr.Retryable = false
	}

	return provErr
}
