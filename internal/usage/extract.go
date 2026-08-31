package usage

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Accumulator observes response payloads without retaining them. Providers
// normally emit one cumulative usage object in the final response/chunk; later
// observations replace earlier values so a stream is never double-counted.
type Accumulator struct {
	latest TokenUsage
	seen   bool
}

// Observe scans one raw JSON or SSE payload for a provider usage object.
func (a *Accumulator) Observe(payload []byte) {
	if a == nil || len(payload) == 0 {
		return
	}
	observed := false
	for _, line := range bytes.Split(bytes.ReplaceAll(payload, []byte("\r"), nil), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if usage, ok := ExtractJSON(line); ok {
			a.latest = usage
			a.seen = true
			observed = true
		}
	}
	if !observed {
		if usage, ok := ExtractJSON(payload); ok {
			a.latest = usage
			a.seen = true
		}
	}
}

// Value returns the latest verified usage observation.
func (a *Accumulator) Value() (TokenUsage, bool) {
	if a == nil || !a.seen {
		return TokenUsage{}, false
	}
	return a.latest, true
}

// ExtractJSON extracts a token breakdown from a JSON response. It supports the
// OpenAI Chat/Responses shapes plus common Anthropic and Gemini usage names.
// A response must contain both input and output counts; cache/reasoning fields
// are optional. Totals-only or partial responses remain unverified so billing
// cannot silently undercount a required dimension.
func ExtractJSON(payload []byte) (TokenUsage, bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || !json.Valid(payload) {
		return TokenUsage{}, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return TokenUsage{}, false
	}
	return findUsage(object, 0)
}

type parsedUsage struct {
	value   TokenUsage
	seen    [5]bool
	invalid bool
}

func findUsage(object map[string]json.RawMessage, depth int) (TokenUsage, bool) {
	if depth > 4 || object == nil {
		return TokenUsage{}, false
	}
	for key, raw := range object {
		normalized := normalizeKey(key)
		switch normalized {
		case "usage", "usagemetadata", "usage_metadata", "tokencounts", "token_counts":
			var usageObject map[string]json.RawMessage
			if json.Unmarshal(raw, &usageObject) == nil {
				if usage, ok := parseUsageObject(usageObject); ok {
					return usage, true
				}
			}
		}
	}
	// Some provider wrappers nest usage one level below a result object. Only
	// recurse into JSON objects, with a small depth bound to avoid scanning an
	// arbitrary response tree indefinitely.
	for _, raw := range object {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) != nil || len(nested) == 0 {
			continue
		}
		if usage, ok := findUsage(nested, depth+1); ok {
			return usage, true
		}
	}
	return TokenUsage{}, false
}

func parseUsageObject(object map[string]json.RawMessage) (TokenUsage, bool) {
	if object == nil {
		return TokenUsage{}, false
	}
	parsed := parsedUsage{}
	parsed.read(&parsed.value.InputTokens, &parsed.seen[0], object,
		"input_tokens", "input_token_count", "inputtokencount", "prompt_tokens", "prompt_token_count", "prompttokencount", "inputtokens", "prompttokens")
	parsed.read(&parsed.value.OutputTokens, &parsed.seen[1], object,
		"output_tokens", "output_token_count", "outputtokencount", "completion_tokens", "candidates_token_count", "candidatestokencount", "outputtokens", "completiontokens")
	parsed.read(&parsed.value.CacheReadTokens, &parsed.seen[2], object,
		"cache_read_tokens", "cache_read_input_tokens", "cached_tokens", "cached_token_count", "cachedcontenttokencount", "cache_read_tokens_count")
	parsed.read(&parsed.value.CacheWriteTokens, &parsed.seen[3], object,
		"cache_write_tokens", "cache_creation_input_tokens", "cache_creation_tokens", "cache_creation_input_token_count")
	parsed.read(&parsed.value.ReasoningTokens, &parsed.seen[4], object,
		"reasoning_tokens", "thoughts_tokens", "thoughts_token_count", "thoughtstokencount", "reasoningtokens", "thoughtstokens")

	for key, raw := range object {
		normalized := normalizeKey(key)
		if normalized != "prompt_tokens_details" && normalized != "input_tokens_details" && normalized != "completion_tokens_details" && normalized != "output_tokens_details" && normalized != "input_token_details" && normalized != "cached_content" {
			continue
		}
		var details map[string]json.RawMessage
		if json.Unmarshal(raw, &details) != nil {
			continue
		}
		parsed.read(&parsed.value.CacheReadTokens, &parsed.seen[2], details,
			"cached_tokens", "cache_read_tokens", "cachedcontenttokencount", "cache_read_input_tokens")
		parsed.read(&parsed.value.CacheWriteTokens, &parsed.seen[3], details,
			"cache_creation_input_tokens", "cache_creation_tokens", "cache_write_tokens")
		parsed.read(&parsed.value.ReasoningTokens, &parsed.seen[4], details,
			"reasoning_tokens", "thoughts_tokens", "reasoningtokens", "thoughtstokens")
	}

	if parsed.invalid {
		return TokenUsage{}, false
	}
	if !parsed.seen[0] || !parsed.seen[1] {
		return TokenUsage{}, false
	}
	return parsed.value, true
}

func (p *parsedUsage) read(target *int64, found *bool, object map[string]json.RawMessage, keys ...string) {
	if *found {
		return
	}
	for key, raw := range object {
		normalized := normalizeKey(key)
		for _, candidate := range keys {
			if normalized != normalizeKey(candidate) {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return
			}
			value, ok := parseNonNegativeInt(raw)
			if ok {
				*target = value
				*found = true
			} else {
				p.invalid = true
			}
			return
		}
	}
}

func parseNonNegativeInt(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		value, err := strconv.ParseInt(string(number), 10, 64)
		if err == nil && value >= 0 {
			return value, true
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err == nil && value >= 0 {
			return value, true
		}
	}
	return 0, false
}

func normalizeKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	return value
}
