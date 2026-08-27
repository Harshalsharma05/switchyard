package logstore

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seed writes n rows directly, bypassing the async writer so the tests below
// are about querying rather than about flush timing.
func seed(t *testing.T, w *Writer, ctx context.Context, recs []Record) {
	t.Helper()
	if got := w.flush(ctx, recs); len(got) != 0 {
		t.Fatalf("flush returned %d rows", len(got))
	}
}

func TestQueryPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	// Identical timestamps on purpose: the id tiebreaker in the cursor is what
	// keeps a page boundary from repeating or skipping a row, and rows written
	// in one batch really can share a millisecond.
	const total = 25
	base := time.Now().UTC().Truncate(time.Millisecond)
	recs := make([]Record, 0, total)
	for i := range total {
		rec := sampleRecord(fmt.Sprintf("req-%02d", i))
		rec.Timestamp = base
		recs = append(recs, rec)
	}
	seed(t, w, ctx, recs)

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		f := Filter{Limit: 10}
		var err error
		if f.Cursor, err = DecodeCursor(cursor); err != nil {
			t.Fatalf("decoding cursor: %v", err)
		}
		page, err := w.Query(ctx, f)
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		for _, r := range page.Records {
			seen[r.ID]++
		}
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Errorf("saw %d distinct rows across %d pages, want %d", len(seen), pages, total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("row %s returned %d times, want once", id, n)
		}
	}
}

func TestQueryFilters(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	base := time.Now().UTC().Truncate(time.Millisecond)
	mk := func(id, team, provider string, status int, at time.Time) Record {
		rec := sampleRecord(id)
		rec.TeamID, rec.Provider, rec.StatusCode, rec.Timestamp = team, provider, status, at
		return rec
	}
	seed(t, w, ctx, []Record{
		mk("a-ok", "acme", "groq", 200, base),
		mk("a-429", "acme", "groq", 429, base.Add(-time.Minute)),
		mk("g-ok", "globex", "ollama", 200, base.Add(-2*time.Minute)),
		mk("g-500", "globex", "ollama", 500, base.Add(-48*time.Hour)),
	})

	tests := map[string]struct {
		filter Filter
		want   []string
	}{
		"by team":          {Filter{TeamID: "acme"}, []string{"a-ok", "a-429"}},
		"by provider":      {Filter{Provider: "ollama"}, []string{"g-ok", "g-500"}},
		"exact status":     {Filter{StatusCode: 429}, []string{"a-429"}},
		"status class 4xx": {Filter{StatusMin: 400, StatusMax: 499}, []string{"a-429"}},
		"team and status":  {Filter{TeamID: "globex", StatusCode: 500}, []string{"g-500"}},
		"since 24h":        {Filter{Since: base.Add(-24 * time.Hour)}, []string{"a-ok", "a-429", "g-ok"}},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			page, err := w.Query(ctx, tt.filter)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := make([]string, 0, len(page.Records))
			for _, r := range page.Records {
				got = append(got, r.ID)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v (newest first)", got, tt.want)
				}
			}
		})
	}
}

// A fallback is findable by what was asked for and by what actually served it
// — searching for either model has to return the row.
func TestQueryModelMatchesRequestedOrServed(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	rec := sampleRecord("fell-back")
	rec.RequestedModel, rec.ServedModel, rec.Fallback = "gpt-oss-120b", "llama3.2:3b", true
	seed(t, w, ctx, []Record{rec})

	for _, model := range []string{"gpt-oss-120b", "llama3.2:3b"} {
		page, err := w.Query(ctx, Filter{Model: model})
		if err != nil {
			t.Fatalf("Query(%s): %v", model, err)
		}
		if len(page.Records) != 1 {
			t.Errorf("model %q matched %d rows, want 1", model, len(page.Records))
		}
	}
}

func TestGetScopedToTeam(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	rec := sampleRecord("owned-by-acme")
	rec.TeamID = "acme"
	seed(t, w, ctx, []Record{rec})

	if _, err := w.Get(ctx, "owned-by-acme", "acme"); err != nil {
		t.Errorf("owning team could not read its own row: %v", err)
	}
	if _, err := w.Get(ctx, "owned-by-acme", "globex"); err != ErrNotFound {
		t.Errorf("another team got %v, want ErrNotFound", err)
	}
	if _, err := w.Get(ctx, "owned-by-acme", ""); err != nil {
		t.Errorf("unscoped admin read failed: %v", err)
	}
}

// The page size ceiling is what stops one request pulling the whole table.
func TestQueryClampsLimit(t *testing.T) {
	pool, ctx := newTestPool(t)
	w := NewWriter(pool, Config{}, nil, discardLogger())

	recs := make([]Record, 0, 3)
	for i := range 3 {
		recs = append(recs, sampleRecord(fmt.Sprintf("r%d", i)))
	}
	seed(t, w, ctx, recs)

	// Asking for more than MaxPageSize must not error; it is clamped.
	if _, err := w.Query(ctx, Filter{Limit: MaxPageSize * 10}); err != nil {
		t.Fatalf("Query with an oversized limit: %v", err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	want := Cursor{TS: time.Now().UTC().Truncate(time.Nanosecond), ID: "abc-123"}
	got, err := DecodeCursor(want.Encode())
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if !got.TS.Equal(want.TS) || got.ID != want.ID {
		t.Errorf("round trip gave %v/%q, want %v/%q", got.TS, got.ID, want.TS, want.ID)
	}
	if _, err := DecodeCursor("not-a-cursor"); err == nil {
		t.Error("a malformed cursor was accepted")
	}
}
