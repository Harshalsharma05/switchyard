// Package cache implements SwitchYard's semantic cache.
//
// Embedding is the first piece: it turns a prompt into a vector so that
// "semantically similar" becomes a number the lookup can threshold on.
package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrEmbedUnavailable marks an embedding failure the caller should treat as a
// cache miss rather than a request failure — the gateway is never the reason a
// request fails.
var ErrEmbedUnavailable = errors.New("embedding unavailable")

// Embedder turns text into a unit-length vector. Defined here because the
// cache is the consumer; whatever implements it is an implementation detail.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimensions() int
}

// EmbedConfig configures a hosted embedding source.
type EmbedConfig struct {
	BaseURL    string
	APIKey     string
	Model      string
	Dimensions int
	Timeout    time.Duration
}

const maxEmbedResponseBytes = 4 << 20

// Gemini calls Google's embedContent endpoint.
//
// Hosted embeddings put a network round trip on the hot path; scripts/embedbench
// exists to keep that cost an argued number rather than an assumption.
type Gemini struct {
	cfg    EmbedConfig
	client *http.Client
}

var _ Embedder = (*Gemini)(nil)

func NewGemini(cfg EmbedConfig) (*Gemini, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("embedding: base_url is required")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("embedding: api key is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("embedding: model is required")
	}
	if cfg.Dimensions <= 0 {
		return nil, fmt.Errorf("embedding: dimensions must be positive, got %d", cfg.Dimensions)
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("embedding: timeout is required")
	}

	// Same transport tuning as internal/provider: the stdlib default of two
	// idle connections per host would spend the latency budget on TLS
	// handshakes.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 50
	t.IdleConnTimeout = 90 * time.Second

	return &Gemini{cfg: cfg, client: &http.Client{Transport: t}}, nil
}

func (e *Gemini) Dimensions() int { return e.cfg.Dimensions }

type embedPart struct {
	Text string `json:"text"`
}

type embedContent struct {
	Parts []embedPart `json:"parts"`
}

type embedRequest struct {
	Model   string       `json:"model"`
	Content embedContent `json:"content"`

	// SEMANTIC_SIMILARITY is the task type Google tunes for "are these two
	// texts asking the same thing", which is exactly the cache's question.
	TaskType string `json:"taskType"`

	OutputDimensionality int `json:"outputDimensionality,omitempty"`
}

type embedResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

// Embed returns a unit-length vector for text. Normalising here means the
// lookup's cosine similarity reduces to a dot product.
func (e *Gemini) Embed(ctx context.Context, text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: empty text", ErrEmbedUnavailable)
	}

	endpoint := strings.TrimSuffix(e.cfg.BaseURL, "/") + "/models/" + url.PathEscape(e.cfg.Model) + ":embedContent"

	body, err := json.Marshal(embedRequest{
		Model:                "models/" + e.cfg.Model,
		Content:              embedContent{Parts: []embedPart{{Text: text}}},
		TaskType:             "SEMANTIC_SIMILARITY",
		OutputDimensionality: e.cfg.Dimensions,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding embedding request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", e.cfg.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: calling %s: %w", ErrEmbedUnavailable, e.cfg.Model, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: reading embedding response: %w", ErrEmbedUnavailable, err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%w: %s returned %d: %s", ErrEmbedUnavailable, e.cfg.Model, resp.StatusCode, truncate(raw, 256))
	}

	var out embedResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: decoding embedding response: %w", ErrEmbedUnavailable, err)
	}

	if len(out.Embedding.Values) != e.cfg.Dimensions {
		return nil, fmt.Errorf("%w: expected %d dimensions, got %d", ErrEmbedUnavailable, e.cfg.Dimensions, len(out.Embedding.Values))
	}

	return Normalize(out.Embedding.Values), nil
}

// Normalize scales a vector to unit length in place, leaving a zero vector
// untouched. Gemini only pre-normalises at its native 3072 dimensions, so a
// truncated output must be normalised here.
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// Similarity is the cosine similarity of two unit vectors, which for
// unit-length input is just their dot product.
func Similarity(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
