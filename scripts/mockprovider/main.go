// Command mockprovider is a standalone stand-in for a real LLM API, speaking
// OpenAI's /chat/completions dialect (JSON and SSE), so scripts/loadtest.js
// can throw thousands of requests at SwitchYard without spending real
// provider money. POST /__control/state flips it into an always-failing
// mode at runtime, which is how the load test's fallback scenario knocks a
// provider over mid-run without killing the process.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"sync/atomic"
	"time"
)

func main() {
	addr := flag.String("addr", ":9500", "listen address")
	name := flag.String("name", "mock", "provider name, echoed into responses")
	latencyMs := flag.Int("latency-ms", 20, "base simulated latency per request")
	jitterMs := flag.Int("jitter-ms", 15, "additional random latency, 0 to this value")
	flag.Parse()

	srv := &server{name: *name, latency: time.Duration(*latencyMs) * time.Millisecond, jitter: time.Duration(*jitterMs) * time.Millisecond}

	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", srv.handleCompletions)
	mux.HandleFunc("/__control/state", srv.handleControl)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Printf("mockprovider %q listening on %s (latency %v+jitter %v)", *name, *addr, srv.latency, srv.jitter)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

type server struct {
	name    string
	latency time.Duration
	jitter  time.Duration
	down    atomic.Bool
}

func (s *server) sleep(r *http.Request) bool {
	d := s.latency
	if s.jitter > 0 {
		d += time.Duration(rand.Int63n(int64(s.jitter)))
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}

type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (s *server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	json.Unmarshal(body, &req)

	if !s.sleep(r) {
		return
	}

	if s.down.Load() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":{"message":%q,"type":"server_error"}}`, s.name+" is down")
		return
	}

	if req.Stream {
		s.writeStream(w)
		return
	}
	s.writeJSON(w)
}

func (s *server) writeJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"load test reply from %s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":24,"completion_tokens":12}}`, s.name)
}

func (s *server) writeStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	chunks := []string{"load ", "test ", "reply ", "from ", s.name}
	for _, c := range chunks {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", c)
		flusher.Flush()
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

type controlState struct {
	Down bool `json:"down"`
}

func (s *server) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var state controlState
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if err := json.Unmarshal(body, &state); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.down.Store(state.Down)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(controlState{Down: s.down.Load()})
}
