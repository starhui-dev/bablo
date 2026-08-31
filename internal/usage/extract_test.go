package usage

import (
	"testing"
)

func TestExtractJSONProviderShapes(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    TokenUsage
	}{
		{
			name:    "openai chat",
			payload: `{"id":"chatcmpl_test","usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
			want:    TokenUsage{InputTokens: 12, OutputTokens: 8, CacheReadTokens: 3, ReasoningTokens: 2},
		},
		{
			name:    "openai responses",
			payload: `{"response":{"usage":{"input_tokens":20,"output_tokens":10,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":4}}}}`,
			want:    TokenUsage{InputTokens: 20, OutputTokens: 10, CacheReadTokens: 7, ReasoningTokens: 4},
		},
		{
			name:    "gemini",
			payload: `{"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":9,"cachedContentTokenCount":5,"thoughtsTokenCount":3}}`,
			want:    TokenUsage{InputTokens: 11, OutputTokens: 9, CacheReadTokens: 5, ReasoningTokens: 3},
		},
		{
			name:    "string counts",
			payload: `{"usage":{"input_tokens":"4","output_tokens":"2"}}`,
			want:    TokenUsage{InputTokens: 4, OutputTokens: 2},
		},
		{
			name:    "optional null details",
			payload: `{"usage":{"prompt_tokens":12,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":null}}}`,
			want:    TokenUsage{InputTokens: 12, OutputTokens: 8},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := ExtractJSON([]byte(test.payload))
			if !ok {
				t.Fatalf("ExtractJSON() found = false, want true")
			}
			if got != test.want {
				t.Fatalf("usage = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestExtractJSONRejectsUnverifiedTotals(t *testing.T) {
	if _, ok := ExtractJSON([]byte(`{"usage":{"completion_tokens":8}}`)); ok {
		t.Fatal("partial token breakdown was treated as verified")
	}
	if _, ok := ExtractJSON([]byte(`{"usage":{"total_tokens":20}}`)); ok {
		t.Fatal("total_tokens-only response was treated as verified")
	}
	if _, ok := ExtractJSON([]byte(`{"usage":{"input_tokens":-1,"output_tokens":2}}`)); ok {
		t.Fatal("negative token count was treated as verified")
	}
	if _, ok := ExtractJSON([]byte(`not-json`)); ok {
		t.Fatal("invalid JSON was treated as verified")
	}
}

func TestAccumulatorUsesLatestCumulativeSSEUsage(t *testing.T) {
	var accumulator Accumulator
	accumulator.Observe([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n"))
	accumulator.Observe([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"))
	got, ok := accumulator.Value()
	if !ok {
		t.Fatal("accumulator did not observe usage")
	}
	want := TokenUsage{InputTokens: 5, OutputTokens: 4}
	if got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}
