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

	// TeamID is carried in the clear alongside the fingerprint that hashes it,
	// because Step 7.4's purge-by-team cannot scan for a hashed value.
	TeamID string
}

// NewKey derives the cache identity for one request on behalf of one team.
//
// Deliberately in the fingerprint: team, requested model, temperature, max
// tokens, stop sequences, and every message before the final one — a follow-up
// turn means something different depending on what preceded it.
//
// Deliberately out: Stream. A streaming and non-streaming request produce the
// same content and differ only in framing, which Step 7.5 handles at delivery.
func NewKey(teamID string, req provider.Request) Key {
	h := sha256.New()

	writeField(h, "team", teamID)
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
		TeamID:      teamID,
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
