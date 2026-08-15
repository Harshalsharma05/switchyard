package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if len(seen) != 32 {
		t.Errorf("generated id = %q, want 32 hex characters", seen)
	}
	if rec.Header().Get(HeaderRequestID) != seen {
		t.Errorf("header = %q, want it to match the context value %q", rec.Header().Get(HeaderRequestID), seen)
	}
}

func TestRequestIDIsUniquePerRequest(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	seen := make(map[string]struct{}, 100)
	for range 100 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		id := rec.Header().Get(HeaderRequestID)
		if _, dup := seen[id]; dup {
			t.Fatalf("id %q was generated twice", id)
		}
		seen[id] = struct{}{}
	}
}

func TestRequestIDHonoursAndSanitizesInbound(t *testing.T) {
	tests := map[string]struct {
		inbound  string
		honoured bool
	}{
		"plain id is kept":         {"abc123", true},
		"dashes and underscores":   {"trace-id_42", true},
		"newline is rejected":      {"abc\ndef", false},
		"carriage return rejected": {"abc\rdef", false},
		"json injection rejected":  {`a","forged":"x`, false},
		"space rejected":           {"abc def", false},
		"overlong rejected":        {strings.Repeat("a", maxRequestIDLen+1), false},
		"empty generates":          {"", false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.inbound != "" {
				req.Header.Set("X-Request-ID", tc.inbound)
			}

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			got := rec.Header().Get(HeaderRequestID)

			if tc.honoured {
				if got != tc.inbound {
					t.Errorf("id = %q, want the inbound %q honoured", got, tc.inbound)
				}
				return
			}

			if got == tc.inbound {
				t.Errorf("id = %q, want a generated replacement rather than the inbound value", got)
			}
			if len(got) != 32 {
				t.Errorf("replacement id = %q, want 32 hex characters", got)
			}
		})
	}
}

// The trap CLAUDE.md calls out first: wrapping a ResponseWriter hides the
// interfaces the original satisfied, and net/http finds http.Flusher by type
// assertion. A logger that breaks this silently disables streaming for every
// handler behind it, which Phase 2 depends on.
func TestLoggerPreservesFlusher(t *testing.T) {
	// Results travel on a buffered channel rather than a shared variable: the
	// handler runs on the server's goroutine, so reading a plain bool from the
	// test goroutine is a data race even when the value happens to be correct.
	flushable := make(chan bool, 1)
	flushed := make(chan bool, 1)

	h := Logger(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		flushable <- ok
		if ok {
			w.Write([]byte("chunk"))
			f.Flush()
		}
		flushed <- ok
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if !<-flushable {
		t.Fatal("http.Flusher was not reachable through the logging wrapper")
	}
	if !<-flushed {
		t.Error("Flush did not complete")
	}
}

// http.ResponseController is the modern route to Flush and deadlines, and it
// finds the real writer by calling Unwrap.
func TestLoggerSupportsResponseController(t *testing.T) {
	controllerWorked := make(chan bool, 1)

	h := Logger(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("chunk"))
		controllerWorked <- http.NewResponseController(w).Flush() == nil
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if !<-controllerWorked {
		t.Error("ResponseController could not reach the underlying writer; Unwrap is missing or wrong")
	}
}

func TestRecovererTurnsPanicIntoFiveHundred(t *testing.T) {
	h := Recoverer(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went badly wrong")
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	// The panic value must not reach the client.
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	if strings.Contains(string(body[:n]), "something went badly wrong") {
		t.Error("panic message leaked into the response body")
	}
}

// The process must survive a panicking handler, since one bad request must not
// take down every other in-flight request.
func TestRecovererKeepsServerServing(t *testing.T) {
	h := Recoverer(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			panic("boom")
		}
		w.WriteHeader(http.StatusOK)
	}))

	srv := httptest.NewServer(h)
	defer srv.Close()

	boom, err := http.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatalf("GET /boom: %v", err)
	}
	boom.Body.Close()

	ok, err := http.Get(srv.URL + "/fine")
	if err != nil {
		t.Fatalf("GET /fine after a panic: %v", err)
	}
	defer ok.Body.Close()

	if ok.StatusCode != http.StatusOK {
		t.Errorf("status = %d after an earlier panic, want 200", ok.StatusCode)
	}
}

func TestRecorderRecordsStatusAndBytes(t *testing.T) {
	t.Run("explicit WriteHeader", func(t *testing.T) {
		rec := &recorder{ResponseWriter: httptest.NewRecorder()}
		rec.WriteHeader(http.StatusTeapot)
		rec.Write([]byte("hello"))

		if rec.status != http.StatusTeapot {
			t.Errorf("status = %d, want 418", rec.status)
		}
		if rec.written != 5 {
			t.Errorf("written = %d, want 5", rec.written)
		}
	})

	t.Run("implicit 200 on bare write", func(t *testing.T) {
		rec := &recorder{ResponseWriter: httptest.NewRecorder()}
		rec.Write([]byte("hi"))

		if rec.status != http.StatusOK {
			t.Errorf("status = %d, want the 200 net/http infers", rec.status)
		}
	})

	t.Run("first status wins", func(t *testing.T) {
		rec := &recorder{ResponseWriter: httptest.NewRecorder()}
		rec.WriteHeader(http.StatusNotFound)
		rec.WriteHeader(http.StatusOK)

		if rec.status != http.StatusNotFound {
			t.Errorf("status = %d, want the first one written", rec.status)
		}
	})
}
