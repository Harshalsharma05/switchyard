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

// The Phase 7 checklist turns entirely on which requests share a fingerprint.
// Anything that changes the meaning of a response must land in a different
// bucket, so a wrong answer here is served confidently rather than caught.
func TestFingerprintSeparation(t *testing.T) {
	tests := map[string]struct {
		mutate   func(*provider.Request)
		teamID   string
		wantSame bool
	}{
		"identical": {
			mutate: func(*provider.Request) {}, teamID: "acme", wantSame: true,
		},
		"different system prompt": {
			mutate: func(r *provider.Request) { r.Messages[0].Content = "You are a pirate." },
			teamID: "acme",
		},
		"different model": {
			mutate: func(r *provider.Request) { r.Model = "openai/gpt-oss-20b" },
			teamID: "acme",
		},
		"different temperature": {
			mutate: func(r *provider.Request) { r.Temperature = f32(0.2) },
			teamID: "acme",
		},
		"temperature unset": {
			mutate: func(r *provider.Request) { r.Temperature = nil },
			teamID: "acme",
		},
		"different max tokens": {
			mutate: func(r *provider.Request) { r.MaxTokens = 1024 },
			teamID: "acme",
		},
		"different stop sequence": {
			mutate: func(r *provider.Request) { r.Stop = []string{"\n\n"} },
			teamID: "acme",
		},
		"different team": {
			mutate: func(*provider.Request) {}, teamID: "globex",
		},
		"different prior turn": {
			mutate: func(r *provider.Request) {
				r.Messages = append([]provider.Message{r.Messages[0],
					{Role: provider.RoleUser, Content: "Hello"},
					{Role: provider.RoleAssistant, Content: "Hi there"},
				}, r.Messages[1])
			},
			teamID: "acme",
		},
		// Streaming is a delivery detail, not a change of meaning: Step 7.5
		// replays the same stored content as chunks.
		"streaming flag": {
			mutate: func(r *provider.Request) { r.Stream = true }, teamID: "acme", wantSame: true,
		},
	}

	want := NewKey("acme", baseRequest())

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := baseRequest()
			tc.mutate(&req)
			got := NewKey(tc.teamID, req)

			if same := got.Fingerprint == want.Fingerprint; same != tc.wantSame {
				t.Fatalf("fingerprint same = %v, want %v", same, tc.wantSame)
			}
		})
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

	if NewKey("acme", unset).Fingerprint == NewKey("acme", zero).Fingerprint {
		t.Fatal("unset temperature must not share a fingerprint with 0.0")
	}
}

func TestQueryExtractionAndNormalization(t *testing.T) {
	req := baseRequest()
	req.Messages[1].Content = "  What is   the capital\n of France?  "

	got := NewKey("acme", req)
	if got.Query != "What is the capital of France?" {
		t.Fatalf("query = %q", got.Query)
	}
	if got.EntryID != NewKey("acme", baseRequest()).EntryID {
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
