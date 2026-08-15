package provider

import "time"

// Config is everything one provider instance needs to make calls. It carries no
// YAML tags on purpose: internal/config owns the file schema and maps it onto
// this struct, so the wire format of configs/providers.yaml can change without
// touching an adapter.
//
// Per-model pricing is deliberately absent. It belongs to the Phase 4 cost
// calculator, not to the adapters, and its representation gets settled in Step
// 1.4 along with the rest of the YAML schema.
type Config struct {
	// Name is the instance name — "openai", "groq", "gemini". It appears in
	// response headers, metrics labels, and errors. Two instances of the same
	// adapter type must have different names.
	Name string

	// BaseURL is the API root, without a trailing slash requirement; adapters
	// normalize it. Making this configurable is what lets Groq stand in for
	// OpenAI with no code change.
	BaseURL string

	// APIKey may be empty — Ollama needs no auth.
	APIKey string

	// Timeout bounds a single request to this provider. It is applied on top of
	// the caller's context, so whichever deadline is sooner wins.
	Timeout time.Duration

	// Models is the set of models this instance serves. It is the source of
	// truth for SupportsModel; no model name is ever hardcoded in Go.
	Models []string

	// DefaultMaxTokens is substituted when a caller omits MaxTokens. Anthropic
	// requires max_tokens outright, and Phase 3's TPM reservation needs a
	// concrete ceiling for every request, so this cannot be left unset.
	DefaultMaxTokens int

	// PingModel is the cheapest model to use for Phase 5 health checks. Empty
	// means "use the first entry in Models".
	PingModel string
}
