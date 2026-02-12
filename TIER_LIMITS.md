# Enchanted AI - Tier Limits & Pricing Reference

## Token Quotas

| Quota Period | Free | Pro | Future Tier |
|--------------|------|-----|-------------|
| **Monthly** | 20,000 | Unlimited | |
| **Weekly** | — | — | |
| **Daily** | — | 500,000 | |
| **Reset Time** | 00:00 UTC (1st of month) | 00:00 UTC (daily) | |

> **Note**: All quota periods enforce independently. Multiple quotas can be active simultaneously.

## Model Access

| Model | Tier Access | Multiplier | Provider | Notes |
|-------|-------------|------------|----------|-------|
| **DeepSeek R1** (`deepseek-r1`) | Free, Pro | **1×** | Tinfoil | |
| **Llama 3.3 70B** (`llama-3.3-70b`) | Free, Pro | **1×** | Tinfoil | |
| **GLM-4.6** (`glm-4.6`) | Free, Pro | **0.6×** | Eternis | Self-hosted |
| **Dolphin Mistral** (`dolphin-mistral`) | Free, Pro | **0.5×** | Eternis | Uncensored, self-hosted |
| **Qwen3 30B** (`qwen3-30b`) | Free, Pro | **0.8×** | NEAR AI | |
| **GPT-4.1** (`gpt-4.1`) | Pro | **4×** | OpenRouter | |
| **GPT-5.2** (`gpt-5.2`) | Pro | **6×** | OpenRouter | |
| **GPT-5.2 Pro** (`gpt-5.2-pro`) | Pro | **70×** | OpenAI | Responses API |
| **GPT-4 Turbo** (`gpt-4-turbo`) | Pro | **1×** | OpenAI | Legacy |
| **Other models** | Pro | **1×** | OpenRouter | Fallback |

### Multiplier Explanation

Plan tokens = Raw tokens × Multiplier

**Examples**:
- A 1,000 token request to GLM-4.6 (0.6× multiplier) counts as **600 plan tokens** against your quota.
- A 1,000 token request to Dolphin Mistral (0.5× multiplier) counts as **500 plan tokens** against your quota.
- A 1,000 token request to GPT-5.2 Pro (70× multiplier) counts as **70,000 plan tokens** against your quota.

## Deep Research

| Limit | Free | Pro | Future Tier |
|-------|------|-----|-------------|
| **Daily Runs** | 0 | 10 | |
| **Lifetime Runs** | 1 | Unlimited | |
| **Token Cap/Run** | 8,000 | 10,000 | |
| **Max Active Sessions** | 1 | 1 | |

> **Note**: Deep research uses GLM-4.6 (0.6× multiplier). Token cap applies to plan tokens.

## Features

| Feature | Free | Pro | Future Tier |
|---------|------|-----|-------------|
| **Document Upload** | ❌ | ✅ | |

> Document-upload blocking on the proxy isn’t working because clients are converting documents to text and sending them as plaintext.
> It will work once client apps will send actual files.
> For now, we’re blocking the feature on the client side, which is sufficient at this stage.

## Cost Calculation Notes

### Key Metrics

1. **Plan Tokens** = Raw Tokens × Model Multiplier
2. **Reset Schedules**:
   - Monthly: 1st of month, 00:00 UTC
   - Weekly: Every Monday, 00:00 UTC
   - Daily: Every day, 00:00 UTC
3. **Enforcement**: All active quotas checked independently (can have both daily + monthly)

### Example Scenarios

#### Free Tier Usage
- **20k monthly quota**: ~20 conversations (1k tokens each) with DeepSeek R1 (1×)
- **OR**: ~40 conversations with Dolphin Mistral (0.5× multiplier, uncensored!)
- **OR**: ~33 conversations with GLM-4.6 (0.6× multiplier)
- **Plus**: 1 deep research run (up to 8k plan tokens)

#### Pro Tier Usage
- **500k daily quota**: ~500 conversations (1k tokens each) with DeepSeek R1 (1×)
- **OR**: ~1,000 conversations with Dolphin Mistral (0.5×) - uncensored model is cost-efficient!
- **OR**: ~833 conversations with GLM-4.6 (0.6×)
- **OR**: ~7 conversations with GPT-5.2 Pro (70×)
- **Plus**: 10 deep research runs/day (up to 10k plan tokens each)

### Tier Design Guidelines

When creating new tiers:

1. **Token Limits**: Consider model multipliers (0.5× to 70×)
2. **Reset Periods**: Choose monthly/weekly/daily based on target usage
3. **Model Access**: Higher tiers = higher multiplier models (premium models use higher multipliers, cheaper/uncensored models use lower multipliers)
4. **Deep Research**: Scale daily runs & token caps together
5. **Features**: Use `AllowedFeatures` array for tier gating
