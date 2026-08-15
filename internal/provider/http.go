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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", cfg.Name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, NewTransportError(cfg.Name, model, err)
	}
	defer resp.Body.Close()

	body, err := readBody(resp.Body)
	// Latency covers the full round trip including reading the body, because
	// that is what the caller actually waited for.
	latency := time.Since(start)
	if err != nil {
		return nil, NewTransportError(cfg.Name, model, err)
	}

	return &httpResult{
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Body:    body,
		Latency: latency,
	}, nil
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
