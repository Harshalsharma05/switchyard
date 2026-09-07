package logstore

import (
	"context"
	"testing"
	"time"
)

func recordN(t *testing.T, a *AuditLog, ctx context.Context, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := a.RecordAudit(ctx, AuditEntry{
			ActorTeamID:  "acme",
			ActorAddr:    "10.0.0.1:5555",
			Action:       "team.patch",
			TargetTeamID: "globex",
			Before:       map[string]any{"rpm": 10},
			After:        map[string]any{"rpm": 20},
		}); err != nil {
			t.Fatalf("RecordAudit %d: %v", i, err)
		}
		// Distinct timestamps so the (ts, id) ordering is deterministic.
		time.Sleep(time.Millisecond)
	}
}

func TestAuditRecordAndList(t *testing.T) {
	pool, ctx := newTestPool(t)
	a := NewAuditLog(pool)

	if err := a.RecordAudit(ctx, AuditEntry{
		ActorTeamID: "acme", ActorAddr: "10.0.0.1:5555",
		Action: "key.rotate", TargetTeamID: "globex",
		Before: map[string]any{"key_source": "config"},
		After:  map[string]any{"key_source": "rotated"},
	}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}

	page, err := a.ListAudit(ctx, 0, "")
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(page.Entries))
	}
	e := page.Entries[0]
	if e.ID == "" || e.Timestamp.IsZero() {
		t.Errorf("entry missing generated id/timestamp: %+v", e)
	}
	if e.Action != "key.rotate" || e.TargetTeamID != "globex" || e.ActorTeamID != "acme" {
		t.Errorf("entry = %+v, want key.rotate acme->globex", e)
	}
	if e.Before["key_source"] != "config" || e.After["key_source"] != "rotated" {
		t.Errorf("before/after = %v / %v, want config -> rotated", e.Before, e.After)
	}
}

// A config.reload entry has no target team: it must round-trip as an empty
// string, not error on the NULL.
func TestAuditNullTargetTeam(t *testing.T) {
	pool, ctx := newTestPool(t)
	a := NewAuditLog(pool)

	if err := a.RecordAudit(ctx, AuditEntry{
		ActorTeamID: "acme", ActorAddr: "10.0.0.1:5555",
		Action: "config.reload",
		After:  map[string]any{"teams": 2, "providers": 3},
	}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}

	page, err := a.ListAudit(ctx, 0, "")
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if page.Entries[0].TargetTeamID != "" {
		t.Errorf("target team = %q, want empty", page.Entries[0].TargetTeamID)
	}
}

// Paging the whole log one small page at a time must return every entry exactly
// once, newest first.
func TestAuditPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	pool, ctx := newTestPool(t)
	a := NewAuditLog(pool)
	recordN(t, a, ctx, 12)

	seen := map[string]bool{}
	var last time.Time
	cursor := ""
	pages := 0
	for {
		page, err := a.ListAudit(ctx, 5, cursor)
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		pages++
		for _, e := range page.Entries {
			if seen[e.ID] {
				t.Fatalf("entry %s returned twice", e.ID)
			}
			seen[e.ID] = true
			if !last.IsZero() && e.Timestamp.After(last) {
				t.Fatalf("ordering broke: %v after %v", e.Timestamp, last)
			}
			last = e.Timestamp
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != 12 {
		t.Errorf("saw %d distinct entries, want 12", len(seen))
	}
	if pages < 3 {
		t.Errorf("paged in %d requests, want at least 3 for 12 rows at page size 5", pages)
	}
}
