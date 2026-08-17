package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestRegistryAuthenticate(t *testing.T) {
	r, err := NewRegistry([]Team{
		{ID: "acme", KeyHash: HashKey("acme-key")},
		{ID: "globex", KeyHash: HashKey("globex-key")},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	team, err := r.Authenticate("acme-key")
	if err != nil {
		t.Fatalf("Authenticate(acme-key): %v", err)
	}
	if team.ID != "acme" {
		t.Errorf("resolved team %q, want acme", team.ID)
	}

	if _, err := r.Authenticate("not-a-real-key"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

func TestRegistryRejectsDuplicateHash(t *testing.T) {
	_, err := NewRegistry([]Team{
		{ID: "acme", KeyHash: HashKey("shared-key")},
		{ID: "globex", KeyHash: HashKey("shared-key")},
	})
	if err == nil {
		t.Fatal("NewRegistry succeeded with two teams sharing a key hash")
	}
	if !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "globex") {
		t.Errorf("error = %v, want it to name both teams", err)
	}
}

func TestRegistryEmptyIsUsable(t *testing.T) {
	r, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}
	if _, err := r.Authenticate("anything"); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

// --- Step 4.3: List/Get/Update -----------------------------------------

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry([]Team{
		{ID: "acme", KeyHash: HashKey("acme-key"), RateLimits: RateLimits{RPM: 60, TPM: 100_000}, MonthlyBudgetMicros: 50_000_000},
		{ID: "globex", KeyHash: HashKey("globex-key"), RateLimits: RateLimits{RPM: 10, TPM: 20_000}, MonthlyBudgetMicros: 5_000_000},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func TestRegistryList(t *testing.T) {
	r := testRegistry(t)

	teams := r.List()
	if len(teams) != 2 {
		t.Fatalf("List() returned %d teams, want 2", len(teams))
	}

	byID := make(map[string]Team, len(teams))
	for _, tm := range teams {
		byID[tm.ID] = tm
	}
	if byID["acme"].RateLimits.RPM != 60 {
		t.Errorf("acme RPM = %d, want 60", byID["acme"].RateLimits.RPM)
	}
	if byID["globex"].MonthlyBudgetMicros != 5_000_000 {
		t.Errorf("globex budget = %d, want 5000000", byID["globex"].MonthlyBudgetMicros)
	}
}

func TestRegistryGet(t *testing.T) {
	r := testRegistry(t)

	team, err := r.Get("acme")
	if err != nil {
		t.Fatalf("Get(acme): %v", err)
	}
	if team.ID != "acme" {
		t.Errorf("Get(acme).ID = %q, want acme", team.ID)
	}

	if _, err := r.Get("nope"); !errors.Is(err, ErrUnknownTeam) {
		t.Errorf("Get(nope) err = %v, want ErrUnknownTeam", err)
	}
}

func TestRegistryUpdateAppliesOnlyPatchedFields(t *testing.T) {
	r := testRegistry(t)

	newRPM := 120
	updated, err := r.Update("acme", TeamPatch{RPM: &newRPM})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.RateLimits.RPM != 120 {
		t.Errorf("RPM = %d, want 120", updated.RateLimits.RPM)
	}
	// TPM and budget were not in the patch, so they must survive unchanged.
	if updated.RateLimits.TPM != 100_000 {
		t.Errorf("TPM = %d, want 100000 (unset fields must not be touched)", updated.RateLimits.TPM)
	}
	if updated.MonthlyBudgetMicros != 50_000_000 {
		t.Errorf("budget = %d, want 50000000 (unset fields must not be touched)", updated.MonthlyBudgetMicros)
	}

	// Get must reflect the update.
	got, err := r.Get("acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RateLimits.RPM != 120 {
		t.Errorf("Get after Update: RPM = %d, want 120", got.RateLimits.RPM)
	}
}

// Update must keep both indexes pointing at the same team, or Authenticate
// would keep resolving stale limits forever after a PATCH.
func TestRegistryUpdateKeepsBothIndexesInSync(t *testing.T) {
	r := testRegistry(t)

	newTPM := 999_999
	if _, err := r.Update("acme", TeamPatch{TPM: &newTPM}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	viaAuth, err := r.Authenticate("acme-key")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if viaAuth.RateLimits.TPM != 999_999 {
		t.Errorf("Authenticate sees TPM = %d after Update, want 999999 (byHash index went stale)", viaAuth.RateLimits.TPM)
	}
}

func TestRegistryUpdateUnknownTeam(t *testing.T) {
	r := testRegistry(t)

	newRPM := 10
	if _, err := r.Update("nope", TeamPatch{RPM: &newRPM}); !errors.Is(err, ErrUnknownTeam) {
		t.Errorf("err = %v, want ErrUnknownTeam", err)
	}
}

func TestRegistryUpdateRejectsInvalidValues(t *testing.T) {
	r := testRegistry(t)

	zero := 0
	negative := -5
	var negBudget int64 = -1

	tests := map[string]TeamPatch{
		"zero rpm":        {RPM: &zero},
		"negative tpm":    {TPM: &negative},
		"negative budget": {MonthlyBudgetMicros: &negBudget},
	}

	for name, patch := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Update("acme", patch); err == nil {
				t.Fatal("Update succeeded, want a validation error")
			}
		})
	}

	// A rejected Update must not have partially applied — acme's original
	// limits must be exactly what testRegistry set up.
	got, err := r.Get("acme")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RateLimits.RPM != 60 || got.RateLimits.TPM != 100_000 || got.MonthlyBudgetMicros != 50_000_000 {
		t.Errorf("acme = %+v, want unchanged after rejected updates", got)
	}
}

// The whole point of the copy-on-write design: a *Team a request already
// holds must never change out from under it, even while concurrent Updates
// and List/Get calls run against the same registry. Run under -race.
func TestRegistryUpdateDoesNotRaceWithReads(t *testing.T) {
	r := testRegistry(t)

	held, err := r.Authenticate("acme-key")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	originalRPM := held.RateLimits.RPM

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 1; i <= 100; i++ {
			rpm := i
			if _, err := r.Update("acme", TeamPatch{RPM: &rpm}); err != nil {
				t.Errorf("Update: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			r.List()
			if _, err := r.Get("acme"); err != nil {
				t.Errorf("Get: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := r.Authenticate("acme-key"); err != nil {
				t.Errorf("Authenticate: %v", err)
			}
		}
	}()

	wg.Wait()

	// The pointer this goroutine grabbed before any Update ran must still
	// report the original value — Update swaps a new pointer into the
	// registry, it never mutates the struct an existing caller is holding.
	if held.RateLimits.RPM != originalRPM {
		t.Errorf("a *Team obtained before any Update changed to RPM=%d, want unchanged %d", held.RateLimits.RPM, originalRPM)
	}
}
