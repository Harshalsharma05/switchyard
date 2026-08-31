package cache

import (
	"context"
	"fmt"
	"strings"
)

// PurgeResult reports what a purge removed.
type PurgeResult struct {
	Scope   string `json:"scope"`
	Target  string `json:"target,omitempty"`
	Entries int    `json:"entries"`
	Indexes int    `json:"indexes"`
}

// purgeBatch bounds how many keys are deleted per round trip. Large enough to
// keep a purge quick, small enough that one DEL never blocks Redis for long —
// this instance also serves rate limiting and budget enforcement.
const purgeBatch = 200

// PurgeTeam removes every entry a team owns.
//
// Fingerprint index membership is deliberately not cleaned up here: those
// entries are gone, and Candidates prunes the dangling IDs the next time the
// bucket is read. Chasing them now would mean scanning every index key to find
// which buckets a deleted entry appeared in.
func (s *Store) PurgeTeam(ctx context.Context, teamID string) (*PurgeResult, error) {
	if strings.TrimSpace(teamID) == "" {
		return nil, fmt.Errorf("purging cache: team id is required")
	}

	tk := teamKey(teamID)
	ids, err := s.rdb.ZRange(ctx, tk, 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("reading team cache index: %w", err)
	}

	removed, err := s.deleteEntries(ctx, ids)
	if err != nil {
		return nil, err
	}

	if err := s.rdb.Del(ctx, tk).Err(); err != nil {
		return nil, fmt.Errorf("deleting team cache index: %w", err)
	}

	return &PurgeResult{Scope: "team", Target: teamID, Entries: removed}, nil
}

// PurgePrefix removes every entry whose ID starts with prefix.
//
// The prefix is validated as hex because an entry ID is a SHA-256 digest, and
// a glob metacharacter reaching the MATCH pattern would delete far more than
// the caller asked for.
func (s *Store) PurgePrefix(ctx context.Context, prefix string) (*PurgeResult, error) {
	if !isHex(prefix) || prefix == "" {
		return nil, fmt.Errorf("purging cache: prefix must be a non-empty hex string")
	}

	ids, err := s.scanKeys(ctx, keyPrefix+":entry:"+prefix+"*", 0)
	if err != nil {
		return nil, err
	}

	removed, err := s.deleteKeys(ctx, ids)
	if err != nil {
		return nil, err
	}
	return &PurgeResult{Scope: "prefix", Target: prefix, Entries: removed}, nil
}

// PurgeAll removes every cache key this package owns, entries and indexes
// alike. Unlike the other two it clears index membership too, since nothing
// is left to prune lazily against.
func (s *Store) PurgeAll(ctx context.Context) (*PurgeResult, error) {
	entries, err := s.scanKeys(ctx, keyPrefix+":entry:*", 0)
	if err != nil {
		return nil, err
	}
	indexes, err := s.scanKeys(ctx, keyPrefix+":index:*", 0)
	if err != nil {
		return nil, err
	}
	teams, err := s.scanKeys(ctx, keyPrefix+":team:*", 0)
	if err != nil {
		return nil, err
	}

	entryCount, err := s.deleteKeys(ctx, entries)
	if err != nil {
		return nil, err
	}
	indexCount, err := s.deleteKeys(ctx, append(indexes, teams...))
	if err != nil {
		return nil, err
	}

	return &PurgeResult{Scope: "all", Entries: entryCount, Indexes: indexCount}, nil
}

// deleteEntries deletes by entry ID rather than by full key.
func (s *Store) deleteEntries(ctx context.Context, ids []string) (int, error) {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = entryKey(id)
	}
	return s.deleteKeys(ctx, keys)
}

func (s *Store) deleteKeys(ctx context.Context, keys []string) (int, error) {
	total := 0
	for start := 0; start < len(keys); start += purgeBatch {
		end := min(start+purgeBatch, len(keys))

		n, err := s.rdb.Del(ctx, keys[start:end]...).Result()
		if err != nil {
			return total, fmt.Errorf("deleting cache keys: %w", err)
		}
		total += int(n)
	}
	return total, nil
}

// scanKeys walks the keyspace with SCAN, never KEYS: this Redis also serves
// rate limiting and budget enforcement, and KEYS blocks it for the whole scan.
func (s *Store) scanKeys(ctx context.Context, pattern string, limit int) ([]string, error) {
	var (
		keys   []string
		cursor uint64
	)

	for {
		batch, next, err := s.rdb.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			return nil, fmt.Errorf("scanning cache keys: %w", err)
		}
		keys = append(keys, batch...)
		cursor = next

		if cursor == 0 || (limit > 0 && len(keys) >= limit) {
			break
		}
	}
	return keys, nil
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
