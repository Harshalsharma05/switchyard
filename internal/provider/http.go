package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/Harshalsharma05/switchyard/internal/telemetry"
)

const (
	// maxResponseBytes caps a non-streaming response body. Without a cap, a
	// broken or hostile upstream can make the gateway allocate without bound —
	// and the gateway must never be the thing that falls over.
	maxResponseBytes = 10 << 20 // 10 MiB

	// maxErrorMessageBytes caps the provider text copied into Error.Message.
	// Error bodies can echo the request, and Error.Message reaches logs.
	maxErrorMessageBytes = 256
)

// newHTTPClient builds a client for one provider instance.
//
// Client.Timeout is deliberately left unset: timeouts are applied per request
// via context, so the caller's deadline and the instance's timeout compose —
// whichever is sooner wins. A Client.Timeout would silently override a shorter
// caller deadline and, worse, cannot be shortened for a single call.
//
// The transport is tuned rather than shared: net/http's default allows only two
// idle connections per host, so under load a gateway spends its time in TCP and
// TLS handshakes. That directly threatens the sub-10ms overhead target.
func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 200
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second

	return &http.Client{Transport: t}
}

// readBody reads a bounded response body.
func readBody(r io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, maxResponseBytes))
}

// httpResult is the raw outcome of one provider round trip.
type httpResult struct {
	Status  int
	Header  http.Header
	Body    []byte
	Latency time.Duration
}

// postJSON marshals payload, POSTs it, and reads a bounded response. Every
// adapter's HTTP mechanics are identical; only the URL, headers, and body
// shapes differ, so those are parameters.
//
// Transport failures come back already classified as *Error. HTTP error
// statuses do not — each adapter classifies those itself, because the response
// body is the only place the retryable-versus-permanent distinction lives.
func postJSON(ctx context.Context, cfg Config, client *http.Client, url, model string, headers map[string]string, payload any) (*httpResult, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding request for %s: %w", cfg.Name, err)
	}

	// Layering the instance timeout onto the caller's context means the sooner
	// of the two deadlines applies. Setting http.Client.Timeout instead would
	// override a shorter caller deadline, which Phase 6 relies on.
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	spanCtx, span := telemetry.Tracer().Start(ctx, "switchyard.provider.http")
	defer span.End()
	span.SetAttributes(semconv.GenAIRequestModel(model))

	req, err := http.NewRequestWithContext(spanCtx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", cfg.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	telemetry.Propagator().Inject(spanCtx, propagation.HeaderCarrier(req.Header))

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, NewTransportError(cfg.Name, model, err)
	}
	defer resp.Body.Close()

	body, err := readBody(resp.Body)
	// Latency covers the full round trip including reading the body, because
	// that is what the caller actually waited for.
	latency := time.Since(start)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, NewTransportError(cfg.Name, model, err)
	}

	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("http %d", resp.StatusCode))
	}

	return &httpResult{
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Body:    body,
		Latency: latency,
	}, nil
}

// openStream starts a streaming POST and hands back the live response without
// reading its body. postJSON cannot be reused here: it reads the whole body
// before returning, which is exactly the buffering Phase 2's design constraint
// forbids for a stream. Ownership of resp.Body passes to the caller, which
// must close it — the StreamReader implementations do this in Close.
//
// Unlike postJSON, this does not wrap ctx in a cfg.Timeout deadline. That
// timeout is sized for one non-streaming round trip; a stream legitimately
// runs far longer while still healthy. The cancellation signal for a stream is
// the caller's own context, which Step 2.3's handler ties to the client
// connection — when the client goes away, ctx.Done() fires and that is what
// stops the upstream call.
func openStream(ctx context.Context, cfg Config, client *http.Client, url, model string, headers map[string]string, payload any) (*http.Response, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding request for %s: %w", cfg.Name, err)
	}

	spanCtx, span := telemetry.Tracer().Start(ctx, "switchyard.provider.http")
	defer span.End()
	span.SetAttributes(semconv.GenAIRequestModel(model))

	req, err := http.NewRequestWithContext(spanCtx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", cfg.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	telemetry.Propagator().Inject(spanCtx, propagation.HeaderCarrier(req.Header))

	resp, err := client.Do(req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, NewTransportError(cfg.Name, model, err)
	}

	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	if resp.StatusCode >= 400 {
		span.SetStatus(codes.Error, fmt.Sprintf("http %d", resp.StatusCode))
	}

	return resp, nil
}

// readStreamError classifies a non-2xx response to a streaming request. It
// exists once here rather than once per adapter because every adapter follows
// the same shape: read the (bounded) body, hand it to the adapter's own
// classify, apply Retry-After. The one difference from postJSON's error path
// is that there was never a stream to preserve, so the body is read in full —
// this is the Step 2.4 "error before the first byte" case, not a mid-stream
// one.
func readStreamError(resp *http.Response, cfg Config, model string, classify func(model string, status int, body []byte) *Error) error {
	defer resp.Body.Close()

	body, err := readBody(resp.Body)
	if err != nil {
		return NewTransportError(cfg.Name, model, err)
	}

	provErr := classify(model, resp.StatusCode, body)
	provErr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	return provErr
}

// parseRetryAfter interprets a Retry-After header, which RFC 9110 allows to be
// either a delay in seconds or an absolute HTTP-date. Zero means the provider
// gave no usable instruction.
//
// now is a parameter rather than a call to time.Now so the date form is
// testable without freezing the clock.
func parseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}

	if secs, err := strconv.Atoi(header); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}

	if t, err := http.ParseTime(header); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}

	// Unparseable. Callers fall back to their own backoff rather than guessing.
	return 0
}

// truncateMessage bounds provider-supplied text and keeps it valid UTF-8, since
// slicing a byte count can land mid-rune.
func truncateMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxErrorMessageBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxErrorMessageBytes], "") + "…"
}
