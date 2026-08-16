package provider

import (
	"io"
	"strings"
	"testing"
)

func TestSSEReaderParsesDataOnlyEvents(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: {\"a\":1}\n\ndata: {\"a\":2}\n\n"))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "" || ev.Data != `{"a":1}` {
		t.Errorf("event = %+v, want data-only event with no event name", ev)
	}

	ev, err = r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Data != `{"a":2}` {
		t.Errorf("Data = %q, want %q", ev.Data, `{"a":2}`)
	}

	if _, err := r.Next(); err != io.EOF {
		t.Errorf("final Next err = %v, want io.EOF", err)
	}
}

func TestSSEReaderParsesNamedEvents(t *testing.T) {
	r := newSSEReader(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "message_start" {
		t.Errorf("Event = %q, want %q", ev.Event, "message_start")
	}
	if ev.Data != `{"type":"message_start"}` {
		t.Errorf("Data = %q", ev.Data)
	}
}

func TestSSEReaderJoinsMultilineData(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: line one\ndata: line two\n\n"))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Data != "line one\nline two" {
		t.Errorf("Data = %q, want multi-line data joined with \\n", ev.Data)
	}
}

// Real upstreams close the connection right after the last event's data,
// without a trailing blank line. A reader that only recognized blank-line
// termination would silently drop that final event.
func TestSSEReaderHandlesTrailingEventWithoutBlankLine(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: {\"a\":1}\n\ndata: {\"a\":2}"))

	if _, err := r.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if ev.Data != `{"a":2}` {
		t.Errorf("Data = %q, want the trailing event to still be read", ev.Data)
	}

	if _, err := r.Next(); err != io.EOF {
		t.Errorf("final Next err = %v, want io.EOF", err)
	}
}

func TestSSEReaderIgnoresCommentsAndUnknownFields(t *testing.T) {
	r := newSSEReader(strings.NewReader(": heartbeat\nid: 5\nretry: 1000\ndata: {\"a\":1}\n\n"))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Data != `{"a":1}` {
		t.Errorf("Data = %q, want the comment and unrecognized fields skipped without error", ev.Data)
	}
}

func TestSSEReaderEmptyStreamIsEOF(t *testing.T) {
	r := newSSEReader(strings.NewReader(""))
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// drainStream reads every chunk from a StreamReader until io.EOF, failing the
// test on any other error. Every adapter's streaming test uses this so the
// drain loop itself is never what's under test.
func drainStream(t *testing.T, s StreamReader) []*Chunk {
	t.Helper()
	defer s.Close()

	var chunks []*Chunk
	for {
		chunk, err := s.Recv()
		if err == io.EOF {
			return chunks
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, chunk)
	}
}
