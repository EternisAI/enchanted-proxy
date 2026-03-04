#!/usr/bin/env python3
"""
GLM-5 SDK Compatibility Test
============================
Tests GLM-5 via NEAR AI using both:
  1. OpenAI Python SDK (streaming) — what our proxy effectively does
  2. Raw httpx SSE — to compare what the wire looks like

Goal: Isolate whether streaming issues are NEAR AI's fault or GLM-5's
non-standard OpenAI-compat responses confusing SDK parsers.

Usage:
    pip install openai httpx
    python3 scripts/glm5_sdk_test.py

Reads NEAR_API_KEY from ../.env
"""

import os
import sys
import json
import time
import httpx
from pathlib import Path
from dataclasses import dataclass, field

# ── Load .env ───────────────────────────────────────────────────────────────

def load_env():
    env_path = Path(__file__).parent.parent / ".env"
    if env_path.exists():
        for line in env_path.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            if not value.startswith("{"):
                os.environ.setdefault(key.strip(), value.strip())

load_env()

BASE_URL = "https://cloud-api.near.ai/v1"
MODEL = "zai-org/GLM-5-FP8"
API_KEY = os.environ.get("NEAR_API_KEY", "")

if not API_KEY:
    print("ERROR: NEAR_API_KEY not found in .env")
    sys.exit(1)

PROMPT = (
    "Write a creative story of approximately 500 words about a robot discovering "
    "music for the first time. The story MUST end with the exact phrase THE END on "
    "its own line as the very last thing you write. Do not add anything after THE END."
)

# ── Colors ──────────────────────────────────────────────────────────────────

class C:
    RED = "\033[0;31m"
    GREEN = "\033[0;32m"
    YELLOW = "\033[0;33m"
    CYAN = "\033[0;36m"
    BOLD = "\033[1m"
    DIM = "\033[2m"
    NC = "\033[0m"

def ok(msg): print(f"  {C.GREEN}✓{C.NC} {msg}")
def fail(msg): print(f"  {C.RED}✗{C.NC} {msg}")
def info(msg): print(f"  {C.CYAN}ℹ{C.NC} {msg}")
def warn(msg): print(f"  {C.YELLOW}⚠{C.NC} {msg}")
def header(msg): print(f"\n{C.BOLD}{'━' * 70}\n  {msg}\n{'━' * 70}{C.NC}")

# ── Test 1: OpenAI Python SDK ──────────────────────────────────────────────

def test_openai_sdk():
    header("Test 1: OpenAI Python SDK (streaming)")

    try:
        from openai import OpenAI
    except ImportError:
        fail("openai package not installed — pip install openai")
        return False

    client = OpenAI(base_url=BASE_URL, api_key=API_KEY)
    info(f"Model: {MODEL}")
    info(f"Base URL: {BASE_URL}")

    reasoning_chunks = 0
    reasoning_chars = 0
    content_chunks = 0
    content_chars = 0
    content_text = ""
    reasoning_text = ""
    finish_reason = None
    first_content_time = None
    first_reasoning_time = None
    errors = []
    chunk_details = []  # Track what the SDK gives us per chunk

    start = time.monotonic()

    try:
        stream = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": PROMPT}],
            stream=True,
        )

        for i, chunk in enumerate(stream):
            elapsed = time.monotonic() - start
            choice = chunk.choices[0] if chunk.choices else None

            if not choice:
                chunk_details.append({"i": i, "t": f"{elapsed:.2f}s", "note": "no choices"})
                continue

            delta = choice.delta

            # Track what fields the SDK exposes
            delta_dict = {}
            if hasattr(delta, "role") and delta.role is not None:
                delta_dict["role"] = delta.role
            if hasattr(delta, "content") and delta.content is not None:
                delta_dict["content"] = delta.content[:50] + "..." if len(delta.content or "") > 50 else delta.content
                content_chunks += 1
                content_chars += len(delta.content)
                content_text += delta.content
                if first_content_time is None:
                    first_content_time = elapsed
            if hasattr(delta, "reasoning_content") and delta.reasoning_content is not None:
                delta_dict["reasoning_content"] = delta.reasoning_content[:50] + "..." if len(delta.reasoning_content or "") > 50 else delta.reasoning_content
                reasoning_chunks += 1
                reasoning_chars += len(delta.reasoning_content)
                reasoning_text += delta.reasoning_content
                if first_reasoning_time is None:
                    first_reasoning_time = elapsed

            # Check for non-standard fields
            for attr in ["reasoning", "tool_calls", "function_call"]:
                val = getattr(delta, attr, None)
                if val is not None:
                    delta_dict[attr] = str(val)[:50]

            if choice.finish_reason:
                finish_reason = choice.finish_reason
                delta_dict["finish_reason"] = finish_reason

            if i < 5 or (i < 20 and delta_dict.get("content")) or choice.finish_reason:
                chunk_details.append({"i": i, "t": f"{elapsed:.2f}s", **delta_dict})

    except Exception as e:
        errors.append(str(e))
        fail(f"SDK error: {e}")

    total_time = time.monotonic() - start

    # ── Report ──────────────────────────────────────────────────────────
    print()
    info(f"Total time: {total_time:.2f}s")
    if first_reasoning_time is not None:
        info(f"Time to first reasoning: {first_reasoning_time:.2f}s")
    if first_content_time is not None:
        info(f"Time to first content: {first_content_time:.2f}s")

    print(f"\n  {C.BOLD}Sample chunks (as seen by SDK):{C.NC}")
    for d in chunk_details[:15]:
        print(f"    {C.DIM}{json.dumps(d)}{C.NC}")
    if len(chunk_details) > 15:
        print(f"    {C.DIM}... ({len(chunk_details)} total logged){C.NC}")

    print()
    if reasoning_chunks > 0:
        ok(f"Reasoning: {reasoning_chunks} chunks, {reasoning_chars} chars")
    else:
        info("No reasoning_content received by SDK")

    if content_chunks > 0:
        ok(f"Content: {content_chunks} chunks, {content_chars} chars")
    else:
        fail("NO content received by SDK!")

    if finish_reason == "stop":
        ok(f"finish_reason: stop")
    elif finish_reason:
        warn(f"finish_reason: {finish_reason}")
    else:
        fail("No finish_reason received")

    if "THE END" in content_text.upper():
        ok('Sentinel "THE END" found')
    elif content_text:
        fail('Sentinel "THE END" NOT found — possible cutoff')
        info(f"Last 200 chars: ...{content_text[-200:]}")

    if errors:
        fail(f"Errors: {errors}")

    passed = content_chunks > 0 and finish_reason == "stop" and not errors
    print(f"\n  {'✅ PASS' if passed else '❌ FAIL'}")
    return passed


# ── Test 2: Raw httpx SSE ───────────────────────────────────────────────────

def test_raw_httpx():
    header("Test 2: Raw httpx SSE (wire format)")

    info(f"Model: {MODEL}")
    info(f"Endpoint: {BASE_URL}/chat/completions")

    body = {
        "model": MODEL,
        "stream": True,
        "messages": [{"role": "user", "content": PROMPT}],
    }

    reasoning_chunks = 0
    content_chunks = 0
    content_text = ""
    reasoning_text = ""
    finish_reason = None
    first_content_time = None
    first_reasoning_time = None
    total_data_lines = 0
    has_done = False
    null_content_chunks = 0
    errors = []

    # Track non-standard fields
    extra_fields_seen = set()
    first_few_chunks = []

    start = time.monotonic()

    try:
        with httpx.Client(timeout=300) as client:
            with client.stream(
                "POST",
                f"{BASE_URL}/chat/completions",
                json=body,
                headers={
                    "Authorization": f"Bearer {API_KEY}",
                    "Content-Type": "application/json",
                },
            ) as resp:
                info(f"HTTP {resp.status_code}")
                if resp.status_code != 200:
                    fail(f"HTTP {resp.status_code}")
                    print(resp.read().decode()[:500])
                    return False

                buffer = ""
                for raw_chunk in resp.iter_text():
                    buffer += raw_chunk
                    while "\n" in buffer:
                        line, buffer = buffer.split("\n", 1)
                        line = line.strip()
                        if not line:
                            continue
                        if line == "data: [DONE]":
                            has_done = True
                            continue
                        if not line.startswith("data: "):
                            if line:
                                warn(f"Non-SSE line: {line[:100]}")
                            continue

                        json_str = line[6:]
                        total_data_lines += 1
                        elapsed = time.monotonic() - start

                        try:
                            d = json.loads(json_str)
                        except json.JSONDecodeError as e:
                            errors.append(f"JSON parse error at chunk {total_data_lines}: {e}")
                            continue

                        choice = d.get("choices", [{}])[0]
                        delta = choice.get("delta", {})

                        # Track ALL keys in delta
                        delta_keys = set(delta.keys())
                        standard_keys = {"role", "content", "reasoning_content", "tool_calls", "function_call"}
                        for k in delta_keys - standard_keys:
                            extra_fields_seen.add(k)

                        # Content analysis
                        c = delta.get("content")
                        rc = delta.get("reasoning_content")

                        if c is not None and c != "":
                            content_chunks += 1
                            content_text += c
                            if first_content_time is None:
                                first_content_time = elapsed
                        elif c is None and rc is None and delta.get("role") is None:
                            # Chunk with nothing useful
                            pass

                        if c is not None and c == "":
                            null_content_chunks += 1

                        if rc is not None and rc != "":
                            reasoning_chunks += 1
                            reasoning_text += rc
                            if first_reasoning_time is None:
                                first_reasoning_time = elapsed

                        fr = choice.get("finish_reason")
                        if fr:
                            finish_reason = fr

                        # Capture first few raw chunks
                        if len(first_few_chunks) < 5:
                            first_few_chunks.append({
                                "i": total_data_lines,
                                "t": f"{elapsed:.2f}s",
                                "delta_keys": sorted(delta.keys()),
                                "content_is_null": c is None,
                                "content_is_empty": c == "",
                                "rc_is_null": rc is None,
                                "finish_reason": fr,
                            })

    except Exception as e:
        errors.append(str(e))
        fail(f"Request error: {e}")

    total_time = time.monotonic() - start

    # ── Report ──────────────────────────────────────────────────────────
    print()
    info(f"Total time: {total_time:.2f}s")
    info(f"Total SSE data chunks: {total_data_lines}")
    if first_reasoning_time is not None:
        info(f"Time to first reasoning: {first_reasoning_time:.2f}s")
    if first_content_time is not None:
        info(f"Time to first content: {first_content_time:.2f}s")

    print(f"\n  {C.BOLD}First few raw chunk shapes:{C.NC}")
    for fc in first_few_chunks:
        print(f"    {C.DIM}{json.dumps(fc)}{C.NC}")

    print()
    if extra_fields_seen:
        warn(f"Non-standard delta fields: {extra_fields_seen}")
    else:
        ok("No non-standard delta fields")

    if null_content_chunks > 0:
        info(f"Chunks with content=\"\" (empty string): {null_content_chunks}")

    if reasoning_chunks > 0:
        ok(f"Reasoning: {reasoning_chunks} chunks, {len(reasoning_text)} chars")

    if content_chunks > 0:
        ok(f"Content: {content_chunks} chunks, {len(content_text)} chars")
    else:
        fail("NO content chunks in raw SSE!")

    if has_done:
        ok("[DONE] marker present")
    else:
        fail("Missing [DONE] marker")

    if finish_reason == "stop":
        ok(f"finish_reason: stop")
    elif finish_reason:
        warn(f"finish_reason: {finish_reason}")
    else:
        fail("No finish_reason")

    if "THE END" in content_text.upper():
        ok('Sentinel "THE END" found')
    elif content_text:
        fail('Sentinel "THE END" NOT found')
        info(f"Last 200 chars: ...{content_text[-200:]}")

    if errors:
        for e in errors:
            fail(f"Error: {e}")

    passed = content_chunks > 0 and finish_reason == "stop" and has_done and not errors
    print(f"\n  {'✅ PASS' if passed else '❌ FAIL'}")
    return passed


# ── Test 3: OpenAI SDK with raw response logging ───────────────────────────

def test_openai_sdk_with_raw():
    """Uses OpenAI SDK but also captures the raw HTTP to compare what the SDK
    sees vs what's on the wire. This catches SDK-level field dropping."""
    header("Test 3: OpenAI SDK — field mapping check")

    try:
        from openai import OpenAI
    except ImportError:
        fail("openai package not installed")
        return False

    client = OpenAI(base_url=BASE_URL, api_key=API_KEY)

    info("Checking which SDK attributes are populated on delta objects...")

    interesting_attrs = []
    content_received = False

    try:
        stream = client.chat.completions.create(
            model=MODEL,
            messages=[{"role": "user", "content": PROMPT}],
            stream=True,
        )

        for i, chunk in enumerate(stream):
            choice = chunk.choices[0] if chunk.choices else None
            if not choice:
                continue
            delta = choice.delta

            # On first chunk, dump ALL attributes
            if i == 0:
                attrs = {k: v for k, v in delta.__dict__.items() if not k.startswith("_")}
                info(f"Delta attributes on chunk 0: {sorted(attrs.keys())}")
                for k, v in sorted(attrs.items()):
                    vtype = type(v).__name__
                    vstr = str(v)[:80] if v is not None else "None"
                    print(f"    {C.DIM}{k}: {vtype} = {vstr}{C.NC}")

            # Check if SDK exposes reasoning_content
            if i < 3:
                has_rc = hasattr(delta, "reasoning_content")
                rc_val = getattr(delta, "reasoning_content", "MISSING")
                interesting_attrs.append({
                    "i": i,
                    "has_reasoning_content_attr": has_rc,
                    "reasoning_content_value": str(rc_val)[:50] if rc_val != "MISSING" else "MISSING",
                    "content": str(delta.content)[:50] if delta.content else None,
                })

            if delta.content:
                content_received = True

            if i > 50:
                break

    except Exception as e:
        fail(f"SDK error: {e}")
        return False

    print(f"\n  {C.BOLD}First chunks attribute check:{C.NC}")
    for a in interesting_attrs:
        print(f"    {C.DIM}{json.dumps(a)}{C.NC}")

    if content_received:
        ok("SDK successfully received content")
    else:
        warn("No content in first 50 chunks (may still be in reasoning phase)")

    return True


# ── Main ────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    print(f"\n{C.BOLD}GLM-5 SDK Compatibility Test{C.NC}")
    print(f"Testing if streaming issues are NEAR AI vs GLM OpenAI-compat\n")

    results = {}

    # Run field mapping check first (quick)
    results["sdk_fields"] = test_openai_sdk_with_raw()

    # Run raw httpx test
    results["raw_httpx"] = test_raw_httpx()

    # Run full OpenAI SDK test
    results["openai_sdk"] = test_openai_sdk()

    # ── Summary ─────────────────────────────────────────────────────────
    header("Summary")
    for name, passed in results.items():
        status = f"{C.GREEN}PASS{C.NC}" if passed else f"{C.RED}FAIL{C.NC}"
        print(f"  {status}  {name}")

    print()
    if results.get("raw_httpx") and not results.get("openai_sdk"):
        warn("Raw SSE works but OpenAI SDK fails → GLM response format not SDK-compatible")
        info("Issue is likely GLM's non-standard fields confusing the OpenAI SDK")
    elif not results.get("raw_httpx") and not results.get("openai_sdk"):
        warn("Both raw and SDK fail → NEAR AI endpoint issue")
    elif results.get("raw_httpx") and results.get("openai_sdk"):
        ok("Both work → issue is likely in our Go proxy's SSE handling")
    print()
