# GLM-5 SSE Diagnostic & Fix

## Background

The OpenAI Swift SDK fails to parse GLM-5 streaming responses from NEAR AI for longer generations. This directory contains the diagnostic tooling used to identify the root cause and verify the proxy fix.

## Root Cause

NEAR AI's GLM-5 (vLLM-based) endpoint crashes mid-generation on longer responses and emits a **non-JSON error line** in the SSE stream:

```
data: error: Failed to perform completion: error decoding response body
data: [DONE]
```

This causes two problems:
1. **Parse failure** — The line is not valid JSON, so any OpenAI-compatible SDK throws a decode error
2. **No finish_reason** — The stream never sends `finish_reason: stop`, so clients don't know the stream ended

Short responses (≤200 tokens) succeed because they hit the token limit (`finish_reason: length`) before the NEAR AI error triggers. The crash appears to be in NEAR AI's vLLM response decoding, not in the model itself.

## Fix

**`internal/streaming/sse_error_normalizer.go`** — Detects non-JSON `data:` lines from upstream providers and converts them into valid OpenAI-format SSE chunks:

```
Input:  data: error: Failed to perform completion: error decoding response body
Output: data: {"id":"error","object":"chat.completion.chunk","created":0,"model":"unknown",
         "choices":[{"index":0,"delta":{"content":"\n\n[Stream error: error: Failed to perform completion: ...]"},
         "finish_reason":"stop"}]}
```

This is wired into both streaming paths (`session.go` and `streaming_simple.go`), alongside the existing `reasoning_content` → `reasoning` field normalizer.

## Test Results (2026-03-02)

### Direct to NEAR AI (no proxy)

| Test | Tokens | Result | Notes |
|------|--------|--------|-------|
| short | 50 | ✅ OK | Hits token limit before crash |
| medium | 200 | ✅ OK | Hits token limit before crash |
| long | 2000 | ❌ FAIL | `data: error:` non-JSON line, no finish_reason |
| reasoning | 2000 | ✅ OK | Hit token limit (reasoning is verbose) |
| very-long | 4000 | ❌ FAIL | Same non-JSON error |

### Through local proxy (with normalizer)

| Test | Tokens | Result | Notes |
|------|--------|--------|-------|
| short | 50 | ✅ OK | Error normalized to valid chunk |
| medium | 200 | ✅ OK | |
| long | 2000 | ✅ OK | Error normalized, `finish_reason: stop` |
| reasoning | 2000 | ✅ OK | |
| very-long | 4000 | ✅ OK | Error normalized, `finish_reason: stop` |

## Diagnostic Script

Swift package in `glm5-diagnostic/` that captures raw SSE output and analyzes each chunk for OpenAI spec compliance.

### Usage

```bash
# From enchanted-proxy/
cd scripts/glm5-funtimes

# Test direct to NEAR AI
swift run GLM5Diagnostic direct

# Test through local proxy (must be running: make run or make run-dev)
swift run GLM5Diagnostic proxy

# Test both and compare
swift run GLM5Diagnostic both
```

Raw SSE files and a summary report are saved to `glm5-diagnostic-output/`.

### What it checks per chunk
- Required top-level fields: `id`, `object`, `created`, `model`, `choices`
- Type correctness (e.g., `created` should be a number)
- Extra/non-standard fields (e.g., `matched_stop`)
- JSON parse failures (the key signal for this bug)
- Content and reasoning accumulation
- `finish_reason` presence

## Related Files

- `internal/streaming/sse_error_normalizer.go` — The fix
- `internal/streaming/sse_error_normalizer_test.go` — Unit tests
- `internal/streaming/reasoning_field_normalizer.go` — Pre-existing related normalizer
- `internal/streaming/session.go` — Session-based streaming (wired in)
- `internal/proxy/streaming_simple.go` — Simple streaming pass-through (wired in)
