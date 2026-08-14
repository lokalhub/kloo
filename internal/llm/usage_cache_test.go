package llm

import (
	"encoding/json"
	"testing"
)

// TestCachedPromptTokensAcrossProviderShapes: providers disagree on how they
// report a prefix-cache hit, so decode both known shapes and normalise them.
func TestCachedPromptTokensAcrossProviderShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			name: "deepseek hit/miss split",
			body: `{"prompt_tokens":1000,"prompt_cache_hit_tokens":768,"prompt_cache_miss_tokens":232}`,
			want: 768,
		},
		{
			name: "openai nested details",
			body: `{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":512}}`,
			want: 512,
		},
		{
			name: "no cache accounting reported",
			body: `{"prompt_tokens":1000,"completion_tokens":50,"total_tokens":1050}`,
			want: 0,
		},
		{
			name: "details present but zero",
			body: `{"prompt_tokens":1000,"prompt_tokens_details":{"cached_tokens":0}}`,
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var u Usage
			if err := json.Unmarshal([]byte(tc.body), &u); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := u.CachedPromptTokens(); got != tc.want {
				t.Errorf("CachedPromptTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestUsageDecodeIsBackwardCompatible: the plain three-field block every
// OpenAI-compatible server sends must still decode unchanged.
func TestUsageDecodeIsBackwardCompatible(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}`), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.PromptTokens != 12 || u.CompletionTokens != 34 || u.TotalTokens != 46 {
		t.Errorf("plain usage decoded wrong: %+v", u)
	}
	if u.PromptTokensDetails != nil {
		t.Error("absent details block should stay nil, not decode to a zero struct")
	}
}
