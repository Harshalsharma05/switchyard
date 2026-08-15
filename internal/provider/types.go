// Package provider defines SwitchYard's canonical request/response format and
// the interface every upstream LLM adapter implements.
//
// Nothing here knows about teams, rate limits, or budgets. This package
// translates SwitchYard's format to and from each provider's dialect and
// classifies the errors they return. That is the whole job.
package provider

import "time"

// Role identifies who authored a message. It is a named string rather than an
// int enum because it crosses the JSON boundary in both directions, and a
// string is readable in logs without a String() method.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is a single turn in a conversation.
//
// Content is a plain string because SwitchYard is text-only in Part 1. Going
// multimodal would make this a slice of typed parts, and every adapter would
// have to handle the new shape.
type Message struct {
	Role    Role
	Content string
}

// Request is the canonical completion request. Adapters translate it into their
// provider's wire format; nothing outside this package builds a
// provider-specific request.
//
// A system prompt is carried as a Message with RoleSystem, not as a dedicated
// field. Providers that want it elsewhere — Anthropic and Gemini both hoist it
// to a top-level field — do that hoisting inside their own adapter, so the
// quirk stays behind the interface instead of spreading into this struct.
type Request struct {
	Model    string
	Messages []Message

	// Temperature is a pointer because 0.0 is a meaningful value (deterministic
	// sampling), so the zero value cannot double as "unset". nil means send no
	// temperature at all and let the provider's own default apply.
	Temperature *float32

	// MaxTokens caps the response length. Zero means unset, and the adapter
	// substitutes the provider's configured default. Unlike Temperature, zero is
	// never a meaningful request, so the zero value is safe as a sentinel here.
	MaxTokens int

	Stream bool
	Stop   []string
}

// FinishReason is the normalized reason generation stopped. Providers spell
// these differently — OpenAI says "stop" and "length", Anthropic says "end_turn"
// and "max_tokens", Gemini says "STOP" and "MAX_TOKENS" — and each adapter maps
// onto this set.
type FinishReason string

const (
	// FinishStop means the model completed naturally or hit a stop sequence.
	FinishStop FinishReason = "stop"
	// FinishLength means generation was truncated by MaxTokens.
	FinishLength FinishReason = "length"
	// FinishContentFilter means the provider refused or redacted the response.
	FinishContentFilter FinishReason = "content_filter"
	// FinishOther covers reasons we deliberately do not model. Adapters use it
	// rather than inventing a new constant, so this set stays small.
	FinishOther FinishReason = "other"
)

// Usage is the token accounting for one request. Phase 4 turns this into cost.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is the canonical completion response.
type Response struct {
	Content      string
	FinishReason FinishReason
	Usage        Usage

	// Model is what the provider actually served, which can differ from what was
	// requested — through an alias, or through a Phase 6 fallback.
	Model string

	// Provider is the instance name from configs/providers.yaml, not the adapter
	// type. Two instances of the same adapter must be distinguishable here.
	Provider string

	// Latency covers the provider round trip only. Gateway overhead is the
	// handler's total elapsed time minus this value, which is why the adapter
	// measures it rather than the proxy: only the adapter knows exactly where
	// the upstream call starts and ends.
	Latency time.Duration
}
