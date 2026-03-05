package streaming

import "strings"

// normalizeReasoningField remaps "reasoning_content" to "reasoning" in SSE data lines.
//
// Some providers (e.g. NEAR AI for GLM 5) use "reasoning_content" as the field name
// for thinking/reasoning output, while clients expect the OpenAI-standard "reasoning" field.
// This performs a simple key rename in the JSON without full parse/serialize overhead.
func normalizeReasoningField(line string) (string, bool) {
	if !strings.Contains(line, `"reasoning_content"`) {
		return line, false
	}
	return strings.Replace(line, `"reasoning_content"`, `"reasoning"`, 1), true
}
