# Pricing for webapp + ios

Created by: Pratyush Tiwari
Created time: November 14, 2025 11:10 PM
Last edited by: Marko Lihter
Last updated time: December 3, 2025 2:43 PM

Concise spec of plan quotas, model cost weightings, and deep research limits.

---

## 1. Plan Structure

### Free tier

- 20,000 **plan tokens/month**
- Default chat model: **GLM-4.6**
- Models:
    - GLM-4.6
    - DeepSeek R1
    - Llama 3.3 70B
- Deep research (GLM-4.6):
    - **1 deep research run total per account (lifetime)**
    - After the first run is submitted, a paywall popup appears:
        
        > "You'll see results of this deep research gathered from 50+ web searches, to continue using deep research subscribe to pro"
        > 
- GPT-5 Pro / other closed-source models: no access

### Premium tier

- **$20/month**
- 500,000 **plan tokens/day**
- Default chat model: **GLM-4.6**
- Models:
    - GLM-4.6
    - DeepSeek R1
    - Llama 3.3 70B
    - GPT-4.1
    - GPT-5
    - GPT-5 Pro
- Deep research (GLM-4.6):
    - **10 deep research runs/day**
    - Per-run cap: **10,000 GLM tokens** (prompt + output, unweighted)

### Research Pack (add-on, TBD details)

- **$10 add-on** on top of Premium
- Additional **deep research capacity** (exact runs and caps TBD)
- Pack remains active until **80% of daily weighted plan-token usage** is reached
(at that point the pack is considered exhausted and can be renewed)
- Final numbers for runs/token caps marked as **TBD**; the 80% exhaustion rule stays fixed

---

## 2. Token accounting based on model costs

- **Plan tokens** = user-visible quota (20k/month, 500k/day)
- **Model tokens** = actual tokens used on a specific model
- Each model token consumes **plan tokens** via a **multiplier** based on relative cost

Baseline:

- DeepSeek R1 and Llama 3.3 70B at **$2 / 1M tokens** are treated as **1× cost**
- 1 DeepSeek/Llama token = **1 plan token**

Multipliers are set to target at least **25% gross margin** under heavy usage, assuming typical OpenRouter and infra pricing.

---

## 3. Per-model cost weighting (multipliers)

Pricing assumptions:

- DeepSeek R1, Llama 3.3 70B: **$2 / 1M tokens**
- GPT-4.1: blended **~$5–$6 / 1M** (input + output)
- GPT-5: blended **~$6–$7 / 1M** (input + output)
- GPT-5 Pro: **$15 / 1M input**, **$120 / 1M output**, **$10 / 1K web search**
(output-heavy usage; web search cost treated as additional overhead)
- GLM-4.6: self-hosted, cost from infra

| Model | Pricing basis (USD, approximate) | Relative cost (approx.) | Plan-token multiplier |
| --- | --- | --- | --- |
| DeepSeek R1 | $2 / 1M tokens | 1× baseline | **1×** |
| Llama 3.3 70B | $2 / 1M tokens | 1× baseline | **1×** |
| GLM-4.6 (self-host) | internal infra, treated as ~$4–$5 / 1M tokens | ~2–2.5× baseline | **3×** |
| GPT-4.1 | blended ~$5–$6 / 1M tokens | ~2.5–3× baseline | **4×** |
| GPT-5 | blended ~$6–$7 / 1M tokens | ~3–3.5× baseline | **6×** |
| GPT-5 Pro | $15 / 1M input, $120 / 1M output, $10 / 1K web search (output-heavy) | ~30–50× baseline (conservative) | **50×** |

Examples:

- 1,000 GLM-4.6 tokens → **3,000 plan tokens**
- 1,000 GPT-4.1 tokens → **4,000 plan tokens**
- 1,000 GPT-5 tokens → **6,000 plan tokens**
- 1,000 GPT-5 Pro tokens → **50,000 plan tokens**

Multipliers can be recalibrated once exact infra and API costs are finalized but should remain sufficiently high to maintain ≥25% margin, especially for GPT-5 Pro due to high output and web-search cost.

---

## 4. GLM-4.6 capacity and defaults

Assumptions:

- Hardware: 8× H200 SXM, tuned for GLM-4.6
- Comfortable performance target per node:
    - ~20 RPS
    - ~3,500 GLM tokens/second
    - Effective budget ≈ **150M GLM tokens/day** at ~50% utilization
- Approximate **100 daily active users**
- GLM-4.6 is the **default model** for both tiers

Under these assumptions:

- Default chat runs on GLM-4.6 for all users
- Deep research limits in Section 1 remain within capacity

Global GLM-4.6 protections (per node):

- Target sustained load:
    - ≤ 20 RPS
    - ≈ 3,500 tokens/second
- If thresholds are exceeded for a sustained period:
    - New deep research jobs are queued or rejected with a “system busy” message

---

## 5. Deep research logic (GLM-4.6)

Deep research is defined as:

- GLM-4.6
- Long-context, multi-step, heavy web search/tool calls
- Higher average token usage per request

Token accounting:

- Deep research tokens = GLM-4.6 model tokens
- Counted in two ways:
    - Against **deep research run limits** (per tier)
    - Against **daily plan tokens**, multiplied by **3×** (GLM-4.6 multiplier)

Run limits summary:

- Free:
    - 1 deep research run **lifetime**
    - Recommended per-run cap: **8,000 GLM tokens**
    - Paywall message shown after first run submission (as in Section 1)
- Premium:
    - 10 deep research runs/day
    - Per-run cap: **10,000 GLM tokens**
    - Max 1 active deep research job per user
- Premium + Research Pack:
    - Additional runs and caps: **TBD**
    - Pack exhausts once **80% of daily weighted plan-token usage** is reached, then can be renewed

---

## 6. GPT-5 Pro access and throttling

- GPT-5 Pro access: **Premium only**
- GPT-5 Pro tokens consume plan tokens with a **50× multiplier**
- High GPT-5 Pro usage rapidly depletes daily plan-token quota
- This multiplier provides a strong throttle on extremely expensive usage while preserving at least **25% gross margin** under heavy GPT-5 Pro usage