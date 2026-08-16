package proxy

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Harshalsharma05/switchyard/internal/provider"
)

// OpenAI allows "stop" as either a string or an array. Mirroring the format
// means mirroring its irregularities.
func TestStopSequencesAcceptsBothForms(t *testing.T) {
	tests := map[string]struct {
		json string
		want stopSequences
	}{
		"single string":  {`"END"`, stopSequences{"END"}},
		"array":          {`["END","STOP"]`, stopSequences{"END", "STOP"}},
		"empty array":    {`[]`, stopSequences{}},
		"null stays nil": {`null`, nil},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var got stopSequences
			if err := json.Unmarshal([]byte(tc.json), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestStopSequencesRejectsNonsense(t *testing.T) {
	var got stopSequences
	if err := json.Unmarshal([]byte(`{"a":1}`), &got); err == nil {
		t.Error("accepted an object, want an error naming the allowed forms")
	}
}

func TestToProviderRequestKeepsSystemMessagesInline(t *testing.T) {
	temp := float32(0)

	req := chatRequest{
		Model: "openai/gpt-oss-120b",
		Messages: []chatMessage{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hello"},
		},
		Temperature: &temp,
		MaxTokens:   256,
		Stop:        stopSequences{"END"},
	}

	got := req.toProviderRequest()

	// Hoisting is the adapter's business. Doing it here would push a provider
	// quirk into the proxy.
	if len(got.Messages) != 2 || got.Messages[0].Role != provider.RoleSystem {
		t.Errorf("messages = %+v, want the system message left in place and first", got.Messages)
	}
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Errorf("temperature = %v, want an explicit 0 to survive", got.Temperature)
	}
	if got.MaxTokens != 256 {
		t.Errorf("max_tokens = %d, want 256", got.MaxTokens)
	}
	if len(got.Stop) != 1 || got.Stop[0] != "END" {
		t.Errorf("stop = %v", got.Stop)
	}
}

func TestWireFinishReason(t *testing.T) {
	tests := map[provider.FinishReason]string{
		provider.FinishStop:          "stop",
		provider.FinishLength:        "length",
		provider.FinishContentFilter: "content_filter",
		// Clients switch on this field and understand only OpenAI's set, so an
		// unmodelled reason becomes "stop" rather than a new token.
		provider.FinishOther: "stop",
	}

	for in, want := range tests {
		t.Run(string(in), func(t *testing.T) {
			if got := wireFinishReason(in); got != want {
				t.Errorf("wireFinishReason(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestToChatResponseDerivesTotalTokens(t *testing.T) {
	got := toChatResponse("abc", 1_700_000_000, &provider.Response{
		Content:      "hi",
		FinishReason: provider.FinishLength,
		Usage:        provider.Usage{InputTokens: 10, OutputTokens: 25},
		Model:        "openai/gpt-oss-120b",
		Provider:     "groq",
	})

	if got.ID != "chatcmpl-abc" {
		t.Errorf("id = %q", got.ID)
	}
	if got.Usage.TotalTokens != 35 {
		t.Errorf("total_tokens = %d, want 35", got.Usage.TotalTokens)
	}
	// The served model is reported, which can differ from what was requested.
	if got.Model != "openai/gpt-oss-120b" {
		t.Errorf("model = %q", got.Model)
	}
	if got.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
}

// --- token estimation (Step 3.3) ---------------------------------------------

func TestEstimateTokens(t *testing.T) {
	tests := map[string]struct {
		req              chatRequest
		defaultMaxTokens int
		want             int
	}{
		"max_tokens supplied is the output ceiling": {
			req: chatRequest{
				Messages:  []chatMessage{{Role: "user", Content: "12345678"}}, // 8 chars -> 2
				MaxTokens: 500,
			},
			defaultMaxTokens: 1024,
			want:             2 + perMessageTokenOverhead + 500,
		},
		"absent max_tokens falls back to the provider default": {
			req: chatRequest{
				Messages: []chatMessage{{Role: "user", Content: "12345678"}},
			},
			defaultMaxTokens: 1024,
			want:             2 + perMessageTokenOverhead + 1024,
		},
		"every message carries its own overhead": {
			req: chatRequest{
				Messages: []chatMessage{
					{Role: "system", Content: "1234"}, // 1
					{Role: "user", Content: "1234"},   // 1
				},
				MaxTokens: 10,
			},
			defaultMaxTokens: 1024,
			want:             (1 + perMessageTokenOverhead) + (1 + perMessageTokenOverhead) + 10,
		},
		"partial token rounds up rather than to zero": {
			req: chatRequest{
				Messages:  []chatMessage{{Role: "user", Content: "hi"}}, // 2 chars -> 1
				MaxTokens: 10,
			},
			defaultMaxTokens: 1024,
			want:             1 + perMessageTokenOverhead + 10,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.req.estimateTokens(tc.defaultMaxTokens); got != tc.want {
				t.Errorf("estimateTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// The reservation is what bounds a team's in-flight exposure, so the estimate
// must never come in under the prompt it is standing in for.
func TestEstimateInputTokensIsNotWildlyLow(t *testing.T) {
	// Roughly 100 words of English, which real tokenizers put near 130 tokens.
	body := ""
	for range 100 {
		body += "sentence "
	}

	req := chatRequest{Messages: []chatMessage{{Role: "user", Content: body}}}
	got := req.estimateInputTokens()

	if got < 100 {
		t.Errorf("estimateInputTokens = %d for a ~130-token prompt; the estimate must not run far under the truth", got)
	}
}
