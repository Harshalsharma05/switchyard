package provider

import (
	"context"
	"sync"
	"time"
)

// Mock is a configurable fake Provider.
//
// It lives in non-test code so packages outside this one can build against it —
// the proxy handler tests need it now, and every resilience test in Phases 6 and
// 7 will. Behaviour is set through plain fields for the common cases and through
// the function hooks when a test needs something a field cannot express.
//
// Mock is safe for concurrent use, which matters because the Phase 3 and Phase
// 11 tests fire many requests at one provider at once.
type Mock struct {
	// ProviderName is what Name reports.
	ProviderName string

	// Models is the set SupportsModel answers for.
	Models []string

	// Response is returned by Complete when CompleteFunc is nil and Err is nil.
	// A nil Response with a nil Err yields a minimal canned reply.
	Response *Response

	// Err is returned by Complete when CompleteFunc is nil. Set it to a *Error
	// to exercise a specific Kind and Retryable combination.
	Err error

	// Delay is slept before Complete returns, for timeout and latency tests. It
	// respects context cancellation rather than blocking past it.
	Delay time.Duration

	// CompleteFunc overrides Response, Err, and Delay entirely when set. It
	// receives the attempt number, starting at 1, so a test can succeed only
	// after N failures.
	CompleteFunc func(ctx context.Context, req Request, attempt int) (*Response, error)

	// PingFunc overrides Ping. When nil, Ping mirrors Complete's outcome.
	PingFunc func(ctx context.Context) error

	mu       sync.Mutex
	attempts int
	requests []Request
}

var _ Provider = (*Mock)(nil)

func (m *Mock) Name() string {
	if m.ProviderName == "" {
		return "mock"
	}
	return m.ProviderName
}

func (m *Mock) SupportsModel(model string) bool {
	for _, candidate := range m.Models {
		if candidate == model {
			return true
		}
	}
	return false
}

func (m *Mock) Complete(ctx context.Context, req Request) (*Response, error) {
	m.mu.Lock()
	m.attempts++
	attempt := m.attempts
	m.requests = append(m.requests, req)
	m.mu.Unlock()

	if m.Delay > 0 {
		// A plain time.Sleep would ignore a cancelled context and make timeout
		// tests hang for the full delay instead of returning promptly.
		select {
		case <-time.After(m.Delay):
		case <-ctx.Done():
			return nil, NewTransportError(m.Name(), req.Model, ctx.Err())
		}
	}

	if m.CompleteFunc != nil {
		return m.CompleteFunc(ctx, req, attempt)
	}
	if m.Err != nil {
		return nil, m.Err
	}
	if m.Response != nil {
		clone := *m.Response
		if clone.Provider == "" {
			clone.Provider = m.Name()
		}
		if clone.Model == "" {
			clone.Model = req.Model
		}
		return &clone, nil
	}

	return &Response{
		Content:      "mock response",
		FinishReason: FinishStop,
		Usage:        Usage{InputTokens: 1, OutputTokens: 2},
		Model:        req.Model,
		Provider:     m.Name(),
		Latency:      m.Delay,
	}, nil
}

func (m *Mock) Stream(context.Context, Request) (StreamReader, error) {
	return nil, ErrStreamingNotImplemented
}

func (m *Mock) Ping(ctx context.Context) error {
	if m.PingFunc != nil {
		return m.PingFunc(ctx)
	}
	model := ""
	if len(m.Models) > 0 {
		model = m.Models[0]
	}
	_, err := m.Complete(ctx, pingRequest(model))
	return err
}

// Attempts reports how many times Complete has been called. Phase 6 uses it to
// assert that an unretryable error produced exactly one attempt.
func (m *Mock) Attempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempts
}

// Requests returns a copy of every request Complete received, in order.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Request, len(m.requests))
	copy(out, m.requests)
	return out
}
