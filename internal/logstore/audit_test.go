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
			ActorUserID:  "usr_1",
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
		ActorUserID: "usr_1", ActorAddr: "10.0.0.1:5555", OrganizationID: "personal",
		Action: "key.rotate", TargetTeamID: "globex",
		Before: map[string]any{"key_source": "config"},
		After:  map[string]any{"key_source": "rotated"},
	}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}

	page, err := a.ListAudit(ctx, AuditQuery{Unscoped: true})
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
	if e.Action != "key.rotate" || e.TargetTeamID != "globex" || e.ActorUserID != "usr_1" {
		t.Errorf("entry = %+v, want key.rotate usr_1->globex", e)
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
		ActorUserID: "usr_1", ActorAddr: "10.0.0.1:5555", OrganizationID: "personal",
		Action: "config.reload",
		After:  map[string]any{"teams": 2, "providers": 3},
	}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}

	page, err := a.ListAudit(ctx, AuditQuery{Unscoped: true})
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
		page, err := a.ListAudit(ctx, AuditQuery{Unscoped: true, Limit: 5, Cursor: cursor})
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

// The read rule, at the query layer. A tenant sees everything filed against its
// own organisation — operator actions on its resources included, because that is
// the case an audit log exists for — and nothing else: not another
// organisation's entries, and not entries belonging to no tenant.
func TestAuditListScopesByOrg(t *testing.T) {
	pool, ctx := newTestPool(t)
	a := NewAuditLog(pool)

	write := func(action, org string, superadmin bool) {
		t.Helper()
		if err := a.RecordAudit(ctx, AuditEntry{
			ActorUserID: "usr_boss", ActorAddr: "10.0.0.1:5555", Action: action,
			OrganizationID: org, Superadmin: superadmin, TargetTeamID: "t",
		}); err != nil {
			t.Fatalf("RecordAudit %s: %v", action, err)
		}
		time.Sleep(time.Millisecond)
	}

	write("own.change", "org-a", false)
	write("other.change", "org-b", false)
	write("operator.change", "org-a", true)
	// An operator action against another tenant: org-a must not see it even
	// though the same operator took it.
	write("operator.elsewhere", "org-b", true)
	// No target organisation — a config reload. Belongs to no tenant.
	write("config.reload", "", true)

	// A row predating Step 2.5 cannot be attributed, so it must not leak into a
	// scoped read just because its column is NULL.
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_log (id, ts, actor_user_id, actor_addr, action, before, after)
		VALUES ('legacy', now(), 'acme', '10.0.0.9:1', 'legacy.change', '{}', '{}')`); err != nil {
		t.Fatalf("seeding a pre-migration row: %v", err)
	}

	scoped, err := a.ListAudit(ctx, AuditQuery{OrgID: "org-a"})
	if err != nil {
		t.Fatalf("scoped ListAudit: %v", err)
	}

	got := map[string]AuditEntry{}
	for _, e := range scoped.Entries {
		got[e.Action] = e
	}
	if len(got) != 2 {
		t.Fatalf("org-a sees %d entries, want 2 (own.change and operator.change): %v", len(got), got)
	}
	if _, ok := got["own.change"]; !ok {
		t.Error("org-a cannot see its own change")
	}

	// The point of the reversal: the tenant sees that an operator acted on its
	// resources.
	op, ok := got["operator.change"]
	if !ok {
		t.Fatal("org-a cannot see an operator's action on its own resources")
	}

	// But not who the operator was.
	if op.ActorUserID != ActorPlatformOperator {
		t.Errorf("actor = %q, want it redacted to %q", op.ActorUserID, ActorPlatformOperator)
	}
	if op.ActorAddr != "" {
		t.Errorf("actor address = %q, want it withheld from a tenant", op.ActorAddr)
	}
	// A tenant's own entries keep their real actor: the redaction is for
	// operator rows only.
	if own := got["own.change"]; own.ActorUserID != "usr_boss" {
		t.Errorf("own entry actor = %q, want the real user usr_boss", own.ActorUserID)
	}

	// A superadmin reads every entry and every real identity.
	all, err := a.ListAudit(ctx, AuditQuery{Unscoped: true})
	if err != nil {
		t.Fatalf("unscoped ListAudit: %v", err)
	}
	if len(all.Entries) != 6 {
		t.Errorf("superadmin sees %d entries, want 6", len(all.Entries))
	}
	for _, e := range all.Entries {
		if e.ActorUserID == ActorPlatformOperator {
			t.Errorf("entry %s redacted the actor for a superadmin read", e.Action)
		}
	}
}
