package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Provider is the contract every upstream adapter satisfies. It is deliberately
// small: anything provider-specific belongs behind it, never on it. If a detail
// of one provider forces a new method here, the abstraction is wrong.
//
// The interface lives in this package rather than in the consuming package,
// which departs from this repo's usual consumer-defines-the-interface rule. The
// reason is mechanical: the registry returns a Provider and lives here, so
// declaring the interface in proxy would make provider import proxy while proxy
// imports provider — a cycle Go rejects at compile time. A consumer that wants a
// narrower view can still declare its own two-method interface; Go satisfies
// interfaces implicitly, so the adapters need no changes to fit it.
type Provider interface {
	// Name is the instance name from configs/providers.yaml.
	Name() string

	// Complete performs a non-streaming completion. It returns a *Error on
	// failure, reachable with errors.As.
	Complete(ctx context.Context, req Request) (*Response, error)

	// Stream performs a streaming completion. The caller must Close the reader.
	Stream(ctx context.Context, req Request) (StreamReader, error)

	// SupportsModel reports whether this instance serves the named model. The
	// source of truth is the model list in configs/providers.yaml, never a slice
	// hardcoded in Go.
	SupportsModel(model string) bool

	// Ping is a minimal liveness call used by the Phase 5 health checker. It
	// returns error rather than *Error so that a nil return is genuinely nil:
	// a nil *Error stored in an error interface compares non-nil, which is the
	// classic Go trap. Callers recover the detail with errors.As.
	Ping(ctx context.Context) error
}

// StreamReader yields response chunks as they arrive. Recv returns io.EOF on
// clean completion. Callers must Close it.
//
// It is declared now, in Phase 1, so that the Provider interface is final and
// Phase 2 does not have to reopen every adapter file to change a signature. The
// per-provider SSE and NDJSON parsing behind it is Phase 2 work.
type StreamReader interface {
	Recv() (*Chunk, error)
	Close() error
}

// Chunk is one normalized increment of a streamed response.
//
// Usage is a pointer because providers send token counts only at the end of a
// stream, and some only when asked — so most chunks legitimately have none, and
// a zero-valued Usage would be indistinguishable from a real count of zero.
type Chunk struct {
	Content      string
	FinishReason FinishReason
	Usage        *Usage
}

// ErrStreamingNotImplemented is returned by Stream until Phase 2 lands. The
// Phase 1 handler rejects stream:true before it ever reaches an adapter; this is
// the backstop so a caller gets a clear error rather than a nil reader.
var ErrStreamingNotImplemented = errors.New("streaming not implemented until phase 2")

// base is the state and the trivial behaviour every adapter shares. Adapters
// embed it, which promotes Name and SupportsModel onto them — Go's composition
// in place of inheritance. Only the genuinely provider-specific methods
// (Complete, Stream, Ping) are then written per adapter.
type base struct {
	cfg    Config
	client *http.Client

	// models is built once at construction and never mutated, so concurrent
	// reads need no lock.
	models map[string]struct{}
}

func newBase(cfg Config) (base, error) {
	if err := validateConfig(cfg); err != nil {
		return base{}, err
	}

	models := make(map[string]struct{}, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m] = struct{}{}
	}

	return base{cfg: cfg, client: newHTTPClient(), models: models}, nil
}

func (b *base) Name() string { return b.cfg.Name }

func (b *base) SupportsModel(model string) bool {
	_, ok := b.models[model]
	return ok
}

// pingModel picks the model the Phase 5 health checker should probe with:
// the configured cheap one, else the first model listed.
func (b *base) pingModel() (string, error) {
	if b.cfg.PingModel != "" {
		return b.cfg.PingModel, nil
	}
	if len(b.cfg.Models) > 0 {
		return b.cfg.Models[0], nil
	}
	return "", fmt.Errorf("pinging %s: no model configured", b.cfg.Name)
}

// pingRequest is the cheapest completion that still proves the provider is
// answering: one token, one word.
func pingRequest(model string) Request {
	return Request{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Content: "ping"}},
		MaxTokens: 1,
	}
}

// malformedResponse builds the error for a 2xx whose body cannot be used.
//
// It is retryable because a garbled success response is usually a transient
// upstream fault — a truncated body, a proxy interfering — and a second attempt
// costs one call.
func malformedResponse(providerName, model, message string, cause error) *Error {
	return &Error{
		Kind:      KindServerError,
		Provider:  providerName,
		Model:     model,
		Retryable: true,
		Message:   message,
		Err:       cause,
	}
}

// validateConfig enforces the invariants every adapter shares. Step 1.4's
// registry calls the constructors at boot, so a malformed config fails startup
// rather than surfacing on the first request.
//
// APIKey is deliberately not required: Ollama takes no auth.
func validateConfig(cfg Config) error {
	switch {
	case cfg.Name == "":
		return errors.New("provider config: name is required")
	case cfg.BaseURL == "":
		return fmt.Errorf("provider %s: base_url is required", cfg.Name)
	case cfg.Timeout <= 0:
		return fmt.Errorf("provider %s: timeout must be positive", cfg.Name)
	case len(cfg.Models) == 0:
		return fmt.Errorf("provider %s: at least one model must be listed", cfg.Name)
	case cfg.DefaultMaxTokens <= 0:
		return fmt.Errorf("provider %s: default_max_tokens must be positive", cfg.Name)
	}
	return nil
}
