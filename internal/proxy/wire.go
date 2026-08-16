// Package proxy orchestrates the request lifecycle: it owns the middleware
// chain, the public HTTP surface, and the translation between SwitchYard's
// public wire format and the canonical provider types.
//
// It is the only package that knows the full lifecycle, and nothing imports it
// except cmd/.
package proxy

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// The public API mirrors OpenAI's chat-completions schema so a client can adopt
// SwitchYard by changing a base URL.
//
// These types are deliberately separate from provider.Request and
// provider.Response. If the canonical types doubled as wire types, OpenAI's
// schema would silently become SwitchYard's internal format and every
// non-OpenAI adapter would inherit its shape.

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float32      `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
	Stop        stopSequences `json:"stop"`
}

// stopSequences accepts both spellings OpenAI allows for "stop": a bare string
// or an array of strings. Mirroring the format means mirroring its
// irregularities, and a client that sends the string form must not get a 400.
type stopSequences []string

func (s *stopSequences) UnmarshalJSON(data []byte) error {
	// A JSON null reaches UnmarshalJSON as the literal "null" rather than being
	// skipped, so without this guard it would decode into a one-element slice
	// holding the empty string.
	if string(data) == "null" {
		return nil
	}

	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = stopSequences{single}
		return nil
	}

	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New("stop must be a string or an array of strings")
	}
	*s = many
	return nil
}

type usageDTO struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   usageDTO     `json:"usage"`
}

// validate rejects requests SwitchYard can reason about as malformed, before a
// provider is chosen. Anything provider-specific is left to the provider, which
// will answer with a KindInvalidRequest the caller sees as a 400 anyway.
func (r chatRequest) validate() error {
	if r.Model == "" {
		return errors.New("model is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("messages must contain at least one message")
	}

	for i, m := range r.Messages {
		switch provider.Role(m.Role) {
		case provider.RoleSystem, provider.RoleUser, provider.RoleAssistant:
		case "":
			return fmt.Errorf("messages[%d]: role is required", i)
		default:
			return fmt.Errorf("messages[%d]: unknown role %q (want system, user, or assistant)", i, m.Role)
		}
	}

	if r.MaxTokens < 0 {
		return errors.New("max_tokens cannot be negative")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return fmt.Errorf("temperature %v is outside the supported range 0 to 2", *r.Temperature)
	}

	return nil
}

// toProviderRequest converts the public shape into the canonical one.
//
// System messages stay in the array: hoisting them is each adapter's business,
// and doing it here would push a provider quirk into the proxy.
func (r chatRequest) toProviderRequest() provider.Request {
	msgs := make([]provider.Message, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, provider.Message{
			Role:    provider.Role(m.Role),
			Content: m.Content,
		})
	}

	return provider.Request{
		Model:       r.Model,
		Messages:    msgs,
		Temperature: r.Temperature,
		MaxTokens:   r.MaxTokens,
		Stream:      r.Stream,
		Stop:        r.Stop,
	}
}

// chatCompletionID builds the public-facing ID from the internal request ID.
// Both the non-streaming response and every chunk of a streaming one carry
// the same value, which is what lets a client correlate a stream's chunks
// with one logical completion — exactly as OpenAI's own IDs do.
func chatCompletionID(requestID string) string {
	return "chatcmpl-" + requestID
}

// toChatResponse converts a canonical response back into the public shape.
func toChatResponse(id string, created int64, resp *provider.Response) chatResponse {
	return chatResponse{
		ID:      chatCompletionID(id),
		Object:  "chat.completion",
		Created: created,
		Model:   resp.Model,
		Choices: []chatChoice{{
			Index: 0,
			Message: chatMessage{
				Role:    string(provider.RoleAssistant),
				Content: resp.Content,
			},
			FinishReason: wireFinishReason(resp.FinishReason),
		}},
		Usage: usageDTO{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
}

// wireFinishReason maps back onto OpenAI's vocabulary.
//
// FinishOther becomes "stop" rather than a new value: clients switch on this
// field and only understand OpenAI's set, so inventing a token here would break
// the drop-in compatibility the wire format exists to provide.
func wireFinishReason(r provider.FinishReason) string {
	switch r {
	case provider.FinishLength:
		return "length"
	case provider.FinishContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

// --- streaming wire shapes ---------------------------------------------------
//
// These mirror OpenAI's chat.completion.chunk objects, the SSE counterpart to
// chatResponse above. The split from chatResponse exists for the same reason
// chatRequest and provider.Request stay separate types: a chunk and a full
// response share some fields but not the same shape, and forcing one type to
// serve both would leak one dialect's quirks into the other.

type chatChunkDelta struct {
	// Role is set only on the first chunk of a stream, omitted on every later
	// one — mirroring OpenAI's own dialect, where a client accumulates the
	// message by starting from an empty one with role "assistant" and only
	// appending Content from every chunk after.
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chatChunkChoice struct {
	Index int            `json:"index"`
	Delta chatChunkDelta `json:"delta"`

	// FinishReason is a pointer so it serializes as JSON null on every chunk
	// but the last, rather than as the empty string OpenAI's own clients do
	// not expect there.
	FinishReason *string `json:"finish_reason"`
}

type chatChunkResponse struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
}

// toChatChunk converts one normalized provider.Chunk into the public wire
// shape. includeRole is true only for the stream's first forwarded chunk; see
// chatChunkDelta.Role.
//
// model is the requested model, not a served one read off the chunk:
// provider.Chunk carries no Model field, because nothing before Phase 6 needs
// it there — a stream has no fallback path yet, so the requested model and
// the served one are always the same.
func toChatChunk(id string, created int64, model string, chunk *provider.Chunk, includeRole bool) chatChunkResponse {
	delta := chatChunkDelta{Content: chunk.Content}
	if includeRole {
		delta.Role = string(provider.RoleAssistant)
	}

	var finish *string
	if chunk.FinishReason != "" {
		s := wireFinishReason(chunk.FinishReason)
		finish = &s
	}

	return chatChunkResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []chatChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}
