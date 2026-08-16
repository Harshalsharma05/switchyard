package provider

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// maxStreamLineBytes bounds a single SSE line or NDJSON line. Without a cap, a
// provider that never sends a newline would make bufio.Scanner grow its buffer
// without limit — the same unbounded-allocation concern maxResponseBytes
// guards against for non-streaming bodies, just applied per line instead of
// per body.
const maxStreamLineBytes = 1 << 20 // 1 MiB

// sseEvent is one Server-Sent Event: an optional named event type and its
// data payload. Multi-line "data:" fields are joined with "\n", per the SSE
// spec; none of this repo's providers use that, but decoding it wrong would
// silently drop content.
type sseEvent struct {
	Event string
	Data  string
}

// sseReader scans an SSE byte stream into events, one per blank-line-delimited
// block. It knows nothing about any provider's JSON payloads — that decoding
// is each adapter's job — so this stays reusable across OpenAI, Gemini, and
// Anthropic despite their differing event shapes.
type sseReader struct {
	scanner *bufio.Scanner
}

func newSSEReader(r io.Reader) *sseReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 4096), maxStreamLineBytes)
	return &sseReader{scanner: s}
}

// Next returns the next event, or io.EOF once the stream ends cleanly. A
// trailing event with no final blank line still counts — real upstreams
// close the connection without one.
func (r *sseReader) Next() (*sseEvent, error) {
	var event string
	var dataLines []string
	sawAny := false

	for r.scanner.Scan() {
		line := r.scanner.Text()

		if line == "" {
			if sawAny {
				return &sseEvent{Event: event, Data: strings.Join(dataLines, "\n")}, nil
			}
			continue
		}
		sawAny = true

		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		default:
			// Comments (lines starting with ":") and fields this gateway does not
			// use (id:, retry:) are ignored rather than treated as an error: a
			// provider is free to add SSE fields, and rejecting the stream over
			// one we don't read would be exactly the kind of gateway-caused
			// failure the design constraints rule out.
		}
	}

	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	if sawAny {
		return &sseEvent{Event: event, Data: strings.Join(dataLines, "\n")}, nil
	}
	return nil, io.EOF
}

// sseStreamReader adapts an sseReader into a provider.StreamReader. Every
// SSE-based adapter (OpenAI-compatible, Anthropic, Gemini) shares this loop;
// only how one event's data decodes into a Chunk differs, which is why decode
// is a field rather than three near-identical copies of this method.
type sseStreamReader struct {
	body   io.ReadCloser
	events *sseReader

	// decode turns one SSE event into a Chunk. A nil Chunk with done=false means
	// the event carried nothing forwardable (a ping, a role-only preamble) and
	// Recv should keep reading. done=true means the provider signalled a clean
	// end (OpenAI's "[DONE]", Anthropic's message_stop) even though more bytes
	// may still be in flight.
	decode func(sseEvent) (chunk *Chunk, done bool, err error)
}

func (r *sseStreamReader) Recv() (*Chunk, error) {
	for {
		ev, err := r.events.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}

		chunk, done, err := r.decode(*ev)
		if err != nil {
			return nil, err
		}
		if done {
			return nil, io.EOF
		}
		if chunk == nil {
			continue
		}
		return chunk, nil
	}
}

func (r *sseStreamReader) Close() error { return r.body.Close() }

// ndjsonStreamReader adapts a newline-delimited-JSON body into a
// provider.StreamReader. Ollama is the only NDJSON dialect here, but this
// stays separate from sseStreamReader rather than folding NDJSON into the SSE
// scanner: the two wire formats have nothing in common beyond "one Recv per
// line", and forcing them through one abstraction would obscure that.
type ndjsonStreamReader struct {
	body    io.ReadCloser
	scanner *bufio.Scanner

	// decode turns one line into a Chunk. Semantics match sseStreamReader.decode.
	decode func(line []byte) (chunk *Chunk, done bool, err error)
}

func (r *ndjsonStreamReader) Recv() (*Chunk, error) {
	for r.scanner.Scan() {
		line := bytes.TrimSpace(r.scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		chunk, done, err := r.decode(line)
		if err != nil {
			return nil, err
		}
		if done {
			return nil, io.EOF
		}
		if chunk == nil {
			continue
		}
		return chunk, nil
	}

	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (r *ndjsonStreamReader) Close() error { return r.body.Close() }
