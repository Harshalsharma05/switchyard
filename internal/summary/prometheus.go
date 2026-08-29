// Hand-rolled Prometheus HTTP API client. Only instant scalar queries are ever
// needed here, and /api/v1/query's response is small and stable, so this
// avoids pulling in client_golang/api and its transitive dependencies.
package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type promClient struct {
	base string
	http *http.Client
}

// promResponse is the subset of /api/v1/query's body this package reads. A
// vector result's samples are [ <unix-ts>, "<value>" ] pairs.
type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// queryScalar runs promql and returns the first sample's value. ok is false
// when the query succeeded but matched nothing, or resolved to NaN/Inf — a
// "no data yet" the caller renders as null, distinct from an error.
func (c *promClient) queryScalar(ctx context.Context, promql string) (value float64, ok bool, err error) {
	u := strings.TrimRight(c.base, "/") + "/api/v1/query?query=" + url.QueryEscape(promql)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false, fmt.Errorf("building prometheus request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("querying prometheus: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("prometheus returned %s", resp.Status)
	}

	var pr promResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return 0, false, fmt.Errorf("decoding prometheus response: %w", err)
	}
	if pr.Status != "success" {
		return 0, false, fmt.Errorf("prometheus query failed: %s", pr.Error)
	}
	if len(pr.Data.Result) == 0 || len(pr.Data.Result[0].Value) != 2 {
		return 0, false, nil
	}

	var raw string
	if err := json.Unmarshal(pr.Data.Result[0].Value[1], &raw); err != nil {
		return 0, false, fmt.Errorf("prometheus sample value was not a string: %w", err)
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parsing prometheus value %q: %w", raw, err)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false, nil
	}
	return v, true, nil
}

// sample is one (timestamp, value) point from a range query.
type sample struct {
	T time.Time
	V float64
}

type promRangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// queryRange runs promql across [start, end] at the given step and returns the
// first series' points. NaN/Inf samples are dropped rather than returned, so a
// window with no data produces a gap the chart can render honestly.
func (c *promClient) queryRange(ctx context.Context, promql string, start, end time.Time, step time.Duration) ([]sample, error) {
	q := url.Values{}
	q.Set("query", promql)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.Itoa(int(step.Seconds())))
	u := strings.TrimRight(c.base, "/") + "/api/v1/query_range?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building prometheus range request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying prometheus range: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %s", resp.Status)
	}

	var pr promRangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return nil, fmt.Errorf("decoding prometheus range response: %w", err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("prometheus range query failed: %s", pr.Error)
	}
	if len(pr.Data.Result) == 0 {
		return nil, nil
	}

	raw := pr.Data.Result[0].Values
	out := make([]sample, 0, len(raw))
	for _, pair := range raw {
		if len(pair) != 2 {
			continue
		}
		var ts float64
		var valStr string
		if json.Unmarshal(pair[0], &ts) != nil || json.Unmarshal(pair[1], &valStr) != nil {
			continue
		}
		v, err := strconv.ParseFloat(valStr, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, sample{T: time.Unix(int64(ts), 0).UTC(), V: v})
	}
	return out, nil
}
