// Black-box proof that streaming stays streaming through the real
// subprocess — chunks arrive progressively rather than buffered — and that
// a client disconnect actually cancels the upstream call instead of leaving
// it running unread.
//
//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamingDeliversChunksProgressively(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	upstream := newMockUpstream(t, providerName)
	team := defaultTeam(uniqueID("acme"), "stream-key", []string{providerName}, []string{model})

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
	}, upstream)

	body := chatBody(model)
	body.Stream = true
	resp := postChat(t, gw, team.key, body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var timestamps []time.Time
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			timestamps = append(timestamps, time.Now())
			if strings.Contains(line, "[DONE]") {
				break
			}
		}
		if err != nil {
			break
		}
	}

	if len(timestamps) < 3 {
		t.Fatalf("received %d SSE events, want at least 3", len(timestamps))
	}

	span := timestamps[len(timestamps)-1].Sub(timestamps[0])
	if span < 50*time.Millisecond {
		t.Errorf("all events arrived within %v; a buffered (non-streamed) response would look exactly like this", span)
	}
}

func TestClientCancelPropagatesToUpstream(t *testing.T) {
	providerName := uniqueID("primary")
	model := uniqueID("model")
	upstream := newMockUpstream(t, providerName)
	cancelled := make(chan struct{})
	upstream.SetHandler(func(w http.ResponseWriter, r *http.Request) {
		writeSSESuccess(w, r, providerName, cancelled)
	})

	team := defaultTeam(uniqueID("acme"), "cancel-key", []string{providerName}, []string{model})

	gw := startGateway(t, harnessConfig{
		providers: []providerSpec{{name: providerName, url: upstream.URL(), models: []string{model}}},
		teams:     []teamSpec{team},
	}, upstream)

	body := chatBody(model)
	body.Stream = true
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling request: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.BaseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+team.key)

	resp, err := gw.Client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 512)
	n, err := resp.Body.Read(buf)
	if n == 0 && err != nil {
		t.Fatalf("reading first chunk: %v", err)
	}

	cancel()

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never observed the client cancellation")
	}
}
