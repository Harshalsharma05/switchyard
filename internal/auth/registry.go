package auth

import (
	"errors"
	"fmt"
)

// ErrUnknownKey means no team's key hash matched the presented credential. It
// is a sentinel so proxy's Auth middleware can map it onto a 401 by identity
// rather than by matching an error string.
var ErrUnknownKey = errors.New("unknown api key")

// Registry resolves a plaintext API key to the team that owns it.
//
// It is indexed by hash, not by ID: the only lookup this package performs is
// "which team does this credential belong to," and every request carries a
// credential, never an ID.
type Registry struct {
	byHash map[string]*Team
}

// NewRegistry indexes teams by key hash.
//
// It fails if two teams share a hash. Two teams cannot share a key: every
// downstream rate limit and budget check assumes a key identifies exactly one
// team, and a collision here would make requests from two teams
// indistinguishable at every layer above this one.
func NewRegistry(teams []Team) (*Registry, error) {
	byHash := make(map[string]*Team, len(teams))

	for _, t := range teams {
		if existing, dup := byHash[t.KeyHash]; dup {
			return nil, fmt.Errorf("teams %q and %q share the same api_key_hash", existing.ID, t.ID)
		}
		team := t
		byHash[t.KeyHash] = &team
	}

	return &Registry{byHash: byHash}, nil
}

// Authenticate resolves a plaintext bearer token to its team.
func (r *Registry) Authenticate(rawKey string) (*Team, error) {
	team, ok := r.byHash[HashKey(rawKey)]
	if !ok {
		return nil, ErrUnknownKey
	}
	return team, nil
}
