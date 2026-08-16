package auth

import (
	"errors"
	"strings"
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
