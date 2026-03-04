#!/usr/bin/env bash
#
# glm5-repro.sh — Minimal reproducer for GLM-5 stream crash on NEAR AI
#
# The stream terminates mid-generation with:
#   data: error: Failed to perform completion: error decoding response body
#
# No finish_reason is ever sent. The model appears to crash during or
# after the reasoning phase, before producing content.
#
# Usage:
#   NEAR_API_KEY=your-key ./scripts/glm5-repro.sh

set -euo pipefail

# Load .env if present
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
if [[ -f "$ENV_FILE" ]]; then
    while IFS='=' read -r key value; do
        [[ -z "$key" || "$key" =~ ^# || "$value" =~ ^\{ ]] && continue
        export "$key=$value" 2>/dev/null || true
    done < "$ENV_FILE"
fi

if [[ -z "${NEAR_API_KEY:-}" ]]; then
    echo "Error: NEAR_API_KEY not set. Export it or add to .env" >&2
    exit 1
fi

OUTFILE="/tmp/glm5-repro.sse"

echo "Calling zai-org/GLM-5-FP8 via NEAR AI (streaming)..."
echo "Output saved to: $OUTFILE"
echo ""

curl -sS --max-time 120 \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $NEAR_API_KEY" \
    -o "$OUTFILE" \
    -w "HTTP %{http_code} | TTFB: %{time_starttransfer}s | Total: %{time_total}s\n" \
    -X POST "https://cloud-api.near.ai/v1/chat/completions" \
    -d '{
        "model": "zai-org/GLM-5-FP8",
        "stream": true,
        "messages": [{"role": "user", "content": "Write a creative story of approximately 500 words about a robot discovering music for the first time. End with THE END."}]
    }'

echo ""

# Show last 10 lines
echo "=== Last 10 lines of SSE stream ==="
tail -10 "$OUTFILE"

echo ""

# Check for the provider error (not inside JSON payloads)
if grep -q '^data: error:' "$OUTFILE"; then
    echo "❌ PROVIDER ERROR in stream:"
    grep '^data: error:' "$OUTFILE"
else
    echo "✓ No provider error lines found"
fi

echo ""

# Check finish_reason
if grep -q '"finish_reason":"stop"' "$OUTFILE" || grep -q '"finish_reason": "stop"' "$OUTFILE"; then
    echo "✓ finish_reason: stop"
else
    echo "❌ No finish_reason: stop — stream ended abnormally"
fi

echo ""
echo "Full raw SSE: $OUTFILE"
