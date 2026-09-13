package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"math"
	"strings"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// keyPrefix namespaces every key this package writes, matching the convention
// internal/budget and internal/health already follow.
const keyPrefix = "switchyard:cache"

// Scope is the tenant boundary a cache entry belongs to: two requests in the
// same scope may be served each other's answers, and nothing ever crosses a
// scope.
//
// Org and Team are both carried because they answer different questions. The
// scope ID — hashed into the fingerprint — decides who shares with whom. Team
// is recorded separately regardless, so purge-by-team still works on an entry
// that was written into a shared organisation cache.
type Scope struct {
	Org  string
	Team string

	// Isolated opts one team out of sharing its organisation's cache, giving it
	// a scope of its own. Set from the team's own configuration.
	Isolated bool
}

// ID is the fingerprint's tenant component.
//
// The org:/team: prefix means an organisation and a team that happen to share
// an ID cannot collide into one scope. An empty Org falls back to team
// scoping rather than pooling every org-less team into one shared "org:"
// bucket — the fail-safe direction, since the alternative would be a
// cross-tenant leak introduced by the very change meant to scope the cache.
func (s Scope) ID() string {
	if s.Isolated || s.Org == "" {
		return "team:" + s.Team
	}
	return "org:" + s.Org
}

// Key is the two-part identity of a cacheable request: a fingerprint covering
// everything that changes the meaning of a response, and the query text that
// gets embedded and compared.
//
// The split is what makes "same prompt, different system prompt" a structural
// miss rather than something the similarity threshold has to notice: entries
// are bucketed by fingerprint, so a different system prompt searches a
// different bucket entirely.
type Key struct {
	Fingerprint string
	Query       string
	EntryID     string

	// TeamID and ScopeID are carried in the clear alongside the fingerprint
	// that hashes the scope, because purge-by-team and the per-tenant entry cap
	// cannot scan for a hashed value.
	TeamID  string
	ScopeID string
}

// NewKey derives the cache identity for one request within one scope.
//
// Deliberately in the fingerprint: the scope, requested model, temperature,
// max tokens, stop sequences, and every message before the final one — a
// follow-up turn means something different depending on what preceded it.
//
// Deliberately out: Stream. A streaming and non-streaming request produce the
// same content and differ only in framing, which Step 7.5 handles at delivery.
func NewKey(scope Scope, req provider.Request) Key {
	h := sha256.New()

	writeField(h, "scope", scope.ID())
	writeField(h, "model", req.Model)
	writeField(h, "maxtok", itoa(req.MaxTokens))

	// nil temperature means "provider default" and 0.0 means "deterministic".
	// They are different requests and must not collide.
	if req.Temperature == nil {
		writeField(h, "temp", "nil")
	} else {
		writeField(h, "temp", ftoa(*req.Temperature))
	}

	for _, s := range req.Stop {
		writeField(h, "stop", s)
	}

	query := ""
	prefix := req.Messages
	if n := len(req.Messages); n > 0 {
		query = normalizeQuery(req.Messages[n-1].Content)
		prefix = req.Messages[:n-1]
	}

	for _, m := range prefix {
		writeField(h, "role", string(m.Role))
		writeField(h, "msg", m.Content)
	}

	fingerprint := hex.EncodeToString(h.Sum(nil))

	// The entry ID is the fingerprint and query hashed together, so the exact
	// tier is a single computed lookup rather than an alias key plus a fetch.
	e := sha256.New()
	writeField(e, "fp", fingerprint)
	writeField(e, "q", query)

	return Key{
		Fingerprint: fingerprint,
		Query:       query,
		EntryID:     hex.EncodeToString(e.Sum(nil)),
		TeamID:      scope.Team,
		ScopeID:     scope.ID(),
	}
}

// EntryKey is the Redis key holding one cached response.
func (k Key) EntryKey() string { return keyPrefix + ":entry:" + k.EntryID }

// IndexKey is the Redis key holding the fingerprint's candidate embeddings.
func (k Key) IndexKey() string { return keyPrefix + ":index:" + k.Fingerprint }

// TeamKey is the Redis key listing every entry a team owns, which is what
// makes purge-by-team possible at all.
func (k Key) TeamKey() string { return teamKey(k.TeamID) }

func teamKey(teamID string) string { return keyPrefix + ":team:" + teamID }

// ScopeKey is the Redis key listing every entry in one scope, which is what the
// per-tenant entry cap counts and trims against. Separate from TeamKey because
// a scope is usually an organisation spanning several teams.
func (k Key) ScopeKey() string { return keyPrefix + ":scope:" + k.ScopeID }

// writeField length-prefixes each field so that no combination of values can
// be rearranged into the same digest — "ab"+"c" must not hash as "a"+"bc".
func writeField(h hash.Hash, name, value string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(name)+len(value)+1))
	h.Write(n[:])
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(value))
}

// normalizeQuery collapses whitespace so that trivial reformatting still hits
// the cheap exact tier. Case is deliberately preserved: the exact tier stays
// literally exact, and a case-only difference falls through to the semantic
// tier, which scores it far above any sane threshold anyway.
func normalizeQuery(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func itoa(n int) string {
	var b [20]byte
	return string(appendInt(b[:0], int64(n)))
}

func appendInt(dst []byte, n int64) []byte {
	if n == 0 {
		return append(dst, '0')
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		dst = append(dst, '-')
	}
	return append(dst, tmp[i:]...)
}

// ftoa renders a float32 by its exact bit pattern rather than a decimal string,
// so two temperatures that print the same but differ in the last bit stay
// distinct requests.
func ftoa(f float32) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], math.Float32bits(f))
	return hex.EncodeToString(b[:])
}
