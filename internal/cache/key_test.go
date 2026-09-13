package cache

import (
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

func f32(v float32) *float32 { return &v }

func baseRequest() provider.Request {
	return provider.Request{
		Model: "gemini-3.5-flash",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "You are a helpful assistant."},
			{Role: provider.RoleUser, Content: "What is the capital of France?"},
		},
		Temperature: f32(0.7),
		MaxTokens:   512,
	}
}

// acmeScope is the baseline: team acme inside organisation personal.
var acmeScope = Scope{Org: "personal", Team: "acme"}

// The Phase 7 checklist turns entirely on which requests share a fingerprint.
// Anything that changes the meaning of a response must land in a different
// bucket, so a wrong answer here is served confidently rather than caught.
//
// The scope cases are Multi-user Step 2.3's: an organisation is the sharing
// boundary, and nothing crosses it.
func TestFingerprintSeparation(t *testing.T) {
	tests := map[string]struct {
		mutate   func(*provider.Request)
		scope    Scope
		wantSame bool
	}{
		"identical": {
			mutate: func(*provider.Request) {}, scope: acmeScope, wantSame: true,
		},
		"different system prompt": {
			mutate: func(r *provider.Request) { r.Messages[0].Content = "You are a pirate." },
			scope:  acmeScope,
		},
		"different model": {
			mutate: func(r *provider.Request) { r.Model = "openai/gpt-oss-20b" },
			scope:  acmeScope,
		},
		"different temperature": {
			mutate: func(r *provider.Request) { r.Temperature = f32(0.2) },
			scope:  acmeScope,
		},
		"temperature unset": {
			mutate: func(r *provider.Request) { r.Temperature = nil },
			scope:  acmeScope,
		},
		"different max tokens": {
			mutate: func(r *provider.Request) { r.MaxTokens = 1024 },
			scope:  acmeScope,
		},
		"different stop sequence": {
			mutate: func(r *provider.Request) { r.Stop = []string{"\n\n"} },
			scope:  acmeScope,
		},
		// A sibling project in the same organisation now shares the cache. This
		// is the one case Step 2.3 deliberately widened: the same person asking
		// the same question from two projects should not pay twice.
		"sibling team in the same org": {
			mutate: func(*provider.Request) {}, scope: Scope{Org: "personal", Team: "globex"},
			wantSame: true,
		},
		// The boundary that matters. A different organisation must never reach
		// this entry, however identical the request.
		"team in a different org": {
			mutate: func(*provider.Request) {}, scope: Scope{Org: "other-org", Team: "globex"},
		},
		// The opt-out: an isolated team gets its own scope even inside its org.
		"isolated team in the same org": {
			mutate: func(*provider.Request) {}, scope: Scope{Org: "personal", Team: "acme", Isolated: true},
		},
		"different prior turn": {
			mutate: func(r *provider.Request) {
				r.Messages = append([]provider.Message{r.Messages[0],
					{Role: provider.RoleUser, Content: "Hello"},
					{Role: provider.RoleAssistant, Content: "Hi there"},
				}, r.Messages[1])
			},
			scope: acmeScope,
		},
		// Streaming is a delivery detail, not a change of meaning: Step 7.5
		// replays the same stored content as chunks.
		"streaming flag": {
			mutate: func(r *provider.Request) { r.Stream = true }, scope: acmeScope, wantSame: true,
		},
	}

	want := NewKey(acmeScope, baseRequest())

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := baseRequest()
			tc.mutate(&req)
			got := NewKey(tc.scope, req)

			if same := got.Fingerprint == want.Fingerprint; same != tc.wantSame {
				t.Fatalf("fingerprint same = %v, want %v", same, tc.wantSame)
			}
		})
	}
}

// A team with no organisation falls back to team scoping. Without this, every
// org-less team would hash to the same "org:" scope and pool their answers —
// a cross-tenant leak introduced by the change meant to prevent one.
func TestOrglessTeamsDoNotShareAScope(t *testing.T) {
	a := NewKey(Scope{Team: "acme"}, baseRequest())
	b := NewKey(Scope{Team: "globex"}, baseRequest())

	if a.Fingerprint == b.Fingerprint {
		t.Fatal("two teams with no organisation must not share a cache scope")
	}
	if a.ScopeID != "team:acme" {
		t.Errorf("scope id = %q, want team:acme", a.ScopeID)
	}
}

// An organisation and a team that happen to share an ID must not collide into
// one scope, which is what the org:/team: prefixes are for.
func TestScopeIDNamespacesOrgAndTeam(t *testing.T) {
	shared := NewKey(Scope{Org: "shared-name", Team: "x"}, baseRequest())
	isolated := NewKey(Scope{Team: "shared-name", Isolated: true}, baseRequest())

	if shared.ScopeID == isolated.ScopeID {
		t.Fatalf("org and team scope ids collided at %q", shared.ScopeID)
	}
}

// The query is what gets embedded, so it must be the final turn alone and must
// survive reformatting that does not change what was asked.
// nil temperature means "provider default" and 0.0 means "deterministic".
// The zero value cannot double as unset, and the fingerprint must agree.
func TestTemperatureNilIsNotZero(t *testing.T) {
	unset, zero := baseRequest(), baseRequest()
	unset.Temperature = nil
	zero.Temperature = f32(0)

	if NewKey(acmeScope, unset).Fingerprint == NewKey(acmeScope, zero).Fingerprint {
		t.Fatal("unset temperature must not share a fingerprint with 0.0")
	}
}

func TestQueryExtractionAndNormalization(t *testing.T) {
	req := baseRequest()
	req.Messages[1].Content = "  What is   the capital\n of France?  "

	got := NewKey(acmeScope, req)
	if got.Query != "What is the capital of France?" {
		t.Fatalf("query = %q", got.Query)
	}
	if got.EntryID != NewKey(acmeScope, baseRequest()).EntryID {
		t.Fatal("whitespace-only reformatting should reach the same entry")
	}
}

func TestVectorRoundTrip(t *testing.T) {
	in := []float32{0, 1, -1, 0.5, 3.14159}
	got := unpackVector(packVector(in))

	if len(got) != len(in) {
		t.Fatalf("length = %d, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("index %d = %v, want %v", i, got[i], in[i])
		}
	}
}
