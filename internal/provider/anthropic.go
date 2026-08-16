package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// anthropicVersion is the API version header Anthropic requires on every
// request. It pins the wire contract, so it belongs in code next to the wire
// types rather than in configs/ — it is not a tunable, and changing it means
// changing the translation below.
const anthropicVersion = "2023-06-01"

// Anthropic adapts Anthropic's /v1/messages API.
//
// Two things make this genuinely different from the OpenAI dialect, and both
// are handled entirely inside this file: the system prompt is a top-level field
// rather than a message, and max_tokens is mandatory rather than optional.
type Anthropic struct {
	base
}

var _ Provider = (*Anthropic)(nil)

func NewAnthropic(cfg Config) (*Anthropic, error) {
	b, err := newBase(cfg)
	if err != nil {
		return nil, err
	}
	return &Anthropic{base: b}, nil
}

// Stream hits the same /v1/messages endpoint as Complete with stream:true.
// Anthropic's dialect is the most different of the three: instead of one
// repeated chunk shape, it sends named events (message_start,
// content_block_delta, message_delta, message_stop, ...), and input token
// usage arrives once at message_start while output token usage arrives at
// message_delta — so the two halves of one Usage are reported roughly a whole
// stream apart. newStreamDecoder closes over inputTokens to bridge that gap.
func (p *Anthropic) Stream(ctx context.Context, req Request) (StreamReader, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/messages"

	headers := map[string]string{
		"anthropic-version": anthropicVersion,
	}
	if p.cfg.APIKey != "" {
		headers["x-api-key"] = p.cfg.APIKey
	}

	resp, err := openStream(ctx, p.cfg, p.client, url, req.Model, headers, p.translateRequest(req, true))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, readStreamError(resp, p.cfg, req.Model, p.classify)
	}

	return &sseStreamReader{
		body:   resp.Body,
		events: newSSEReader(resp.Body),
		decode: p.newStreamDecoder(req.Model),
	}, nil
}

func (p *Anthropic) Ping(ctx context.Context) error {
	model, err := p.pingModel()
	if err != nil {
		return err
	}
	_, err = p.Complete(ctx, pingRequest(model))
	return err
}

func (p *Anthropic) Complete(ctx context.Context, req Request) (*Response, error) {
	url := strings.TrimSuffix(p.cfg.BaseURL, "/") + "/messages"

	headers := map[string]string{
		"anthropic-version": anthropicVersion,
	}
	if p.cfg.APIKey != "" {
		// Anthropic authenticates with x-api-key, not a bearer token.
		headers["x-api-key"] = p.cfg.APIKey
	}

	res, err := postJSON(ctx, p.cfg, p.client, url, req.Model, headers, p.translateRequest(req, false))
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

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model    string             `json:"model"`
	Messages []anthropicMessage `json:"messages"`

	// MaxTokens has no omitempty: Anthropic rejects the request outright if it
	// is missing. This is why Config.DefaultMaxTokens has to exist.
	MaxTokens int `json:"max_tokens"`

	// System is the hoisted system prompt, omitted when there was none.
	System string `json:"system,omitempty"`

	Temperature   *float32 `json:"temperature,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`
	Stream        bool     `json:"stream,omitempty"`
}

type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicErrorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- stream event wire types -------------------------------------------------
//
// Each is only the slice of its event's JSON this adapter reads. Anthropic's
// stream events carry more fields (content_block_start's block type, ping's
// empty body, message_start's full initial message); the rest is left
// unparsed rather than modeled, since nothing here needs it.

type anthropicMessageStartEvent struct {
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type anthropicContentBlockDeltaEvent struct {
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type anthropicMessageDeltaEvent struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// --- translation ------------------------------------------------------------

// translateRequest hoists system messages out of the array into the top-level
// system field.
//
// This is the classic Anthropic gotcha, and containing it here is the whole
// argument for keeping the system prompt as an ordinary Message in the
// canonical Request: OpenAI and Ollama want it inline, Anthropic and Gemini
// want it hoisted, and only the adapters that need the hoist perform it.
func (p *Anthropic) translateRequest(req Request, stream bool) anthropicRequest {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = p.cfg.DefaultMaxTokens
	}

	var systemParts []string
	msgs := make([]anthropicMessage, 0, len(req.Messages))

	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			// Multiple system messages, or one out of first position, are
			// concatenated in order — the documented edge case, since Anthropic
			// accepts exactly one system value.
			systemParts = append(systemParts, m.Content)
			continue
		}
		msgs = append(msgs, anthropicMessage{Role: string(m.Role), Content: m.Content})
	}

	return anthropicRequest{
		Model:         req.Model,
		Messages:      msgs,
		MaxTokens:     maxTokens,
		System:        strings.Join(systemParts, "\n\n"),
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Stream:        stream,
	}
}

func (p *Anthropic) translateResponse(model string, raw []byte, latency time.Duration) (*Response, error) {
	var out anthropicResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, malformedResponse(p.cfg.Name, model, "upstream returned a body that is not a valid message", err)
	}
	if len(out.Content) == 0 {
		return nil, malformedResponse(p.cfg.Name, model, "upstream returned no content blocks", nil)
	}

	// Content arrives as a list of typed blocks. Only text blocks matter in
	// Part 1; anything else (tool use, thinking) is skipped rather than
	// rendered, so it can never leak into the response body as noise.
	var content strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			content.WriteString(block.Text)
		}
	}

	served := out.Model
	if served == "" {
		served = model
	}

	return &Response{
		Content:      content.String(),
		FinishReason: anthropicFinishReason(out.StopReason),
		Usage: Usage{
			InputTokens:  out.Usage.InputTokens,
			OutputTokens: out.Usage.OutputTokens,
		},
		Model:    served,
		Provider: p.cfg.Name,
		Latency:  latency,
	}, nil
}

func anthropicFinishReason(s string) FinishReason {
	switch s {
	case "end_turn", "stop_sequence":
		return FinishStop
	case "max_tokens":
		return FinishLength
	case "refusal":
		return FinishContentFilter
	default:
		return FinishOther
	}
}

// newStreamDecoder builds the decode closure sseStreamReader drives. It is a
// closure rather than a plain method because it must carry inputTokens across
// events: message_start reports it once, near the start of the stream, and it
// has to be remembered until message_delta reports the matching output count,
// near the end.
func (p *Anthropic) newStreamDecoder(model string) func(sseEvent) (*Chunk, bool, error) {
	var inputTokens int

	return func(ev sseEvent) (*Chunk, bool, error) {
		data := strings.TrimSpace(ev.Data)
		if data == "" {
			return nil, false, nil
		}

		switch ev.Event {
		case "message_start":
			var wire anthropicMessageStartEvent
			if err := json.Unmarshal([]byte(data), &wire); err != nil {
				return nil, false, fmt.Errorf("decoding %s stream event: %w", p.cfg.Name, err)
			}
			inputTokens = wire.Message.Usage.InputTokens
			return nil, false, nil

		case "content_block_delta":
			var wire anthropicContentBlockDeltaEvent
			if err := json.Unmarshal([]byte(data), &wire); err != nil {
				return nil, false, fmt.Errorf("decoding %s stream event: %w", p.cfg.Name, err)
			}
			// Only text_delta carries forwardable content in Part 1's text-only
			// world; other delta types (input_json_delta, for tool use) never
			// occur here because SwitchYard never sends tools.
			if wire.Delta.Type != "text_delta" {
				return nil, false, nil
			}
			return &Chunk{Content: wire.Delta.Text}, false, nil

		case "message_delta":
			var wire anthropicMessageDeltaEvent
			if err := json.Unmarshal([]byte(data), &wire); err != nil {
				return nil, false, fmt.Errorf("decoding %s stream event: %w", p.cfg.Name, err)
			}
			chunk := &Chunk{
				Usage: &Usage{InputTokens: inputTokens, OutputTokens: wire.Usage.OutputTokens},
			}
			if wire.Delta.StopReason != "" {
				chunk.FinishReason = anthropicFinishReason(wire.Delta.StopReason)
			}
			return chunk, false, nil

		case "message_stop":
			return nil, true, nil

		case "error":
			// Anthropic can send this mid-stream on a 200 response — a failure
			// that never touched HTTP status, which is exactly why StreamReader
			// surfaces it as a Recv error rather than relying on the status check
			// Stream already did before the reader was even built.
			var wire anthropicErrorEnvelope
			if err := json.Unmarshal([]byte(data), &wire); err != nil {
				wire = anthropicErrorEnvelope{}
			}
			return nil, false, &Error{
				Kind:      KindServerError,
				Provider:  p.cfg.Name,
				Model:     model,
				Retryable: true,
				Message:   truncateMessage(wire.Error.Message),
			}

		default:
			// ping, content_block_start, content_block_stop, and any future event
			// type: nothing here is forwardable content.
			return nil, false, nil
		}
	}
}

func (p *Anthropic) classify(model string, status int, body []byte) *Error {
	var env anthropicErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		env = anthropicErrorEnvelope{}
	}

	message := truncateMessage(env.Error.Message)
	if message == "" {
		message = truncateMessage(string(body))
	}

	provErr := NewHTTPError(p.cfg.Name, model, status, message)

	// Anthropic returns 529 for "overloaded", which is outside the usual 5xx
	// range some clients check but is very much worth retrying. KindForStatus
	// already treats anything >= 500 as a server error, so this only needs
	// asserting, not special-casing.
	if env.Error.Type == "overloaded_error" {
		provErr.Kind = KindServerError
		provErr.Retryable = true
	}

	return provErr
}
