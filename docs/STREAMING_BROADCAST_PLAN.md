# Streaming Broadcast Architecture Plan

## Table of Contents
1. [Overview](#overview)
2. [Current Architecture Analysis](#current-architecture-analysis)
3. [Requirements](#requirements)
4. [Proposed Architecture](#proposed-architecture)
5. [Tool Execution Architecture](#tool-execution-architecture)
6. [Implementation Phases](#implementation-phases)
7. [Detailed Component Design](#detailed-component-design)
8. [API Design](#api-design)
9. [Edge Cases & Error Handling](#edge-cases--error-handling)
10. [Testing Strategy](#testing-strategy)
11. [Rollout Plan](#rollout-plan)
12. [Future Enhancements](#future-enhancements)

---

## Overview

### Problem Statement
Currently, when a client disconnects during AI response streaming (e.g., user closes browser tab), the proxy stops reading from the upstream AI provider. This causes:
- **Incomplete responses** - Only partial answers are stored in Firestore
- **Wasted compute** - AI provider already processed the request but response is lost
- **No multi-viewer support** - Multiple clients cannot watch the same response stream in real-time

### Goals
1. **Resilient streaming** - Continue reading from AI provider even if all clients disconnect
2. **Multi-client broadcast** - Allow multiple clients to watch the same response simultaneously
3. **User control** - Enable users to stop generation mid-stream, with all viewers notified
4. **Complete message storage** - Always store full responses with all tool calls (including partial responses when stopped)
5. **Late-join support** - Clients can subscribe to in-progress streams and get the full response (30min window)
6. **Simplified client integration** - Remove X-BASE-URL requirement, automatic model routing
7. **Unified message storage** - Proxy handles both user and AI message storage
8. **Backward compatibility** - Existing clients continue to work without changes

### Success Metrics
- ✅ 100% message completion rate (no partial responses stored)
- ✅ Support 10+ concurrent viewers per stream without performance degradation
- ✅ Graceful handling of all-clients-disconnect scenario
- ✅ Zero breaking changes to existing client apps
- ✅ Clients don't need to maintain model-to-provider mapping
- ✅ Atomic user+AI message storage in Firestore

---

## Current Architecture Analysis

### Current Flow (cmd/server/main.go + internal/proxy/handlers.go)

```
Client Request
    ↓
Proxy Handler (gin.HandlerFunc)
    ↓
httputil.ReverseProxy → OpenAI/Claude
    ↓
ModifyResponse → handleStreamingResponse()
    ↓
┌─────────────────────────────────────┐
│ for scanner.Scan() {                │
│   select {                          │
│     case <-ctx.Done():              │ ← Client disconnect stops loop
│       return // PROBLEM!            │
│   }                                 │
│   pw.Write(line) // Write to client│ ← Write failure stops loop
│   accumulate content                │
│ }                                   │
└─────────────────────────────────────┘
    ↓
Save to Firestore (may be incomplete)
```

### Issues with Current Approach

1. **Client-coupled upstream reading** (`handlers.go:264-268`)
   ```go
   select {
   case <-ctx.Done():  // ctx = c.Request.Context() (client lifecycle)
       log.Debug("client disconnected, stopping stream processing")
       return  // Stops reading from OpenAI
   default:
   }
   ```
   **Problem**: Request context is cancelled when client disconnects, so we stop reading upstream.

2. **Write failures are fatal** (`handlers.go:273-276`)
   ```go
   if _, err := pw.Write(append([]byte(line), '\n')); err != nil {
       log.Error("failed to write to pipe", slog.String("error", err.Error()))
       return  // Stops reading from OpenAI
   }
   ```
   **Problem**: If client can't receive data fast enough, we abandon the entire upstream read.

3. **No stream sharing**
   - Each client request creates a separate upstream request to OpenAI
   - Concurrent clients viewing same chat hit OpenAI multiple times
   - No way to join an in-progress response stream

4. **Message storage depends on client connection**
   - `saveMessageAsync()` called at end of stream processing
   - If loop exits early due to client disconnect, message may be incomplete

---

## Requirements

### Functional Requirements

| ID | Requirement | Priority | Current Status |
|----|-------------|----------|----------------|
| FR1 | Proxy must complete upstream read even if client disconnects | CRITICAL | ❌ Not met |
| FR2 | Multiple clients can subscribe to same active stream | HIGH | ❌ Not met |
| FR3 | Late joiners receive full response from beginning | MEDIUM | ❌ Not met |
| FR4 | Store complete responses with all tool calls | CRITICAL | ⚠️ Partial (only if client stays connected) |
| FR5 | Existing API endpoints continue to work | CRITICAL | ✅ Must maintain |
| FR6 | Client A disconnect doesn't affect Client B's stream | HIGH | ❌ Not met |

### Non-Functional Requirements

| ID | Requirement | Priority | Target |
|----|-------------|----------|--------|
| NFR1 | Stream latency | HIGH | <100ms overhead vs. current |
| NFR2 | Memory efficiency | HIGH | <10MB per active stream |
| NFR3 | Concurrent streams | MEDIUM | Support 100+ active streams |
| NFR4 | Cleanup of stale streams | MEDIUM | Auto-cleanup after 30min of completion |
| NFR5 | Graceful degradation | HIGH | If broadcast fails, fall back to direct proxy |

---

## Proposed Architecture

### High-Level Design

```
┌─────────────────────────────────────────────────────────────────┐
│                         PROXY LAYER                              │
│                                                                  │
│  Client A Request ─┐                                            │
│  Client B Request ─┼─→ Proxy Handler                            │
│  Client C Request ─┘    ↓                                       │
│                         Check: Active stream for this           │
│                         chat_id + message_id?                   │
│                         ↓                                       │
│                    ┌────┴─────┐                                │
│                    │   YES    │    NO                           │
│                    ↓          ↓                                 │
│            Subscribe to   Create new                            │
│            existing      StreamSession                          │
│            session           ↓                                  │
│                └──────┬──────┘                                  │
│                       ↓                                         │
│              ┌─────────────────┐                                │
│              │ StreamSession   │                                │
│              │ (per response)  │                                │
│              └────────┬────────┘                                │
│                       │                                         │
│        ┌──────────────┼──────────────┐                         │
│        ↓              ↓              ↓                          │
│   Subscriber A   Subscriber B   Subscriber C                    │
│   (Client A)     (Client B)     (Client C)                      │
│        │              │              │                          │
└────────┼──────────────┼──────────────┼──────────────────────────┘
         ↓              ↓              ↓
    SSE Stream     SSE Stream     SSE Stream
         ↓              ↓              ↓
    Client A       Client B       Client C


┌─────────────────────────────────────────────────────────────────┐
│                      STREAM SESSION                              │
│                                                                  │
│  ┌──────────────────┐                                           │
│  │ Upstream Reader  │ (goroutine)                               │
│  │ (background ctx) │                                           │
│  └────────┬─────────┘                                           │
│           ↓                                                     │
│    Read from OpenAI/Claude                                      │
│    (continues even if all clients disconnect)                   │
│           ↓                                                     │
│    ┌──────────────┐                                            │
│    │ Chunk Buffer │ (stores all chunks)                        │
│    │ []StreamChunk│                                            │
│    └──────┬───────┘                                            │
│           │                                                     │
│           ├─────→ Broadcast to Subscriber A                    │
│           ├─────→ Broadcast to Subscriber B                    │
│           └─────→ Broadcast to Subscriber C                    │
│                                                                  │
│  When complete:                                                 │
│    → Extract full content + tool calls                          │
│    → Save to Firestore                                         │
│    → Keep session alive for 30min (late joiners)               │
│    → Auto-cleanup                                              │
└─────────────────────────────────────────────────────────────────┘
```

### Key Architectural Decisions

#### Decision 1: Background Context for Upstream Reading
**Rationale**: Use stoppable context independent of client requests to ensure upstream reading completes regardless of client lifecycle, while allowing graceful cancellation when needed.

```go
// ❌ Current (bad)
ctx := c.Request.Context()  // Tied to client
for scanner.Scan() {
    select {
    case <-ctx.Done():
        return  // Client disconnect stops reading
    }
}

// ✅ Proposed (good)
// Create stoppable context from the start
stopCtx, stopCancel := context.WithCancel(context.Background())
// With optional timeout
ctx := stopCtx
if timeout > 0 {
    ctx, _ = context.WithTimeout(stopCtx, 30*time.Minute)
}
defer stopCancel()

for scanner.Scan() {
    select {
    case <-ctx.Done():
        // Stopped by user or timeout
        if errors.Is(ctx.Err(), context.Canceled) {
            // User-initiated stop
        } else {
            // Timeout
        }
        return
    default:
    }
    // Process chunk
}
```

**Key Benefits**:
- Independent of client lifecycle (client disconnect doesn't affect upstream)
- Supports user-initiated stop via `stopCancel()`
- Supports timeout via `WithTimeout`
- Distinguishes between cancellation reasons

#### Decision 2: Session-Based Stream Management
**Rationale**: Create session abstraction identified by `(chatID, messageID)` to enable stream sharing and resumption.

**Why not use `chatID` alone?**
- Multiple concurrent messages in same chat (e.g., regenerate response)
- `messageID` uniquely identifies one AI response

**Why not use connection IDs?**
- Connections are ephemeral
- Need stable identifier that clients can reference

#### Decision 3: Buffered Broadcast Pattern
**Rationale**: Store chunks in memory buffer for:
- Late-join replay (client joins mid-stream, gets full response)
- Message extraction after completion
- Debugging and observability

**Memory concerns**:
- Average AI response: ~1000 tokens = ~4KB
- 100 active streams × 10KB average = 1MB total
- Auto-cleanup after 30min keeps memory bounded

#### Decision 4: Non-Blocking Broadcast
**Rationale**: Slow client shouldn't block fast clients or upstream reading.

```go
// Non-blocking send with timeout
select {
case sub.Ch <- chunk:
    // Sent successfully
case <-time.After(100 * time.Millisecond):
    // Subscriber too slow, skip this chunk (they'll get next one)
    log.Warn("subscriber lagging")
}
```

**Trade-off**: Slow clients may miss chunks, but:
- They can request full message from Firestore after completion
- Prevents memory buildup and deadlocks
- Protects fast clients and upstream reading

---

## Tool Execution Architecture

### Overview

When AI models request tool/function calls during streaming responses, we need a strategy for executing these tools. This is particularly important in multi-viewer scenarios where multiple clients are watching the same stream.

### Two Types of Tool Execution

We design for two distinct execution patterns, though **initially we only implement server-side tools**.

#### 1. Server-Side Tools (Remote Tools)

**Definition**: Tools executed on the proxy/backend server without requiring client device access.

**Characteristics**:
- Stateless operations (web APIs, calculations, database queries)
- No device-specific context needed
- Results are identical regardless of execution location
- Can be retried on failure
- Centrally logged and monitored

**Examples**:
- `web_search` - Query Exa AI, SerpAPI, or Brave Search
- `get_weather` - Call weather API
- `database_query` - Query application database
- `calculate` - Mathematical operations
- `image_generation` - Call DALL-E or Stable Diffusion
- `fetch_url` - HTTP GET request
- `send_notification` - Push notification via FCM/APNs

**Benefits**:
- ✅ Executes exactly once (no duplication in multi-viewer scenarios)
- ✅ Works even if all clients disconnect
- ✅ Centralized logging and monitoring
- ✅ Can implement retries and rate limiting
- ✅ Secure credential management (API keys on server)

**Considerations**:
- ⚠️ Cannot access client device capabilities
- ⚠️ Requires sandboxing for security (e.g., code execution tools)
- ⚠️ May need quota/rate limiting per user

#### 2. Client-Side Tools (Local/On-Device Tools) - Future

**Definition**: Tools requiring access to client device capabilities or local context.

**Characteristics**:
- Platform-specific APIs (iOS/Android/Web)
- Requires user permissions
- Access to local resources (files, camera, location)
- Results may vary by device

**Examples** (Not Implemented Yet):
- `read_local_file` - Access device file system
- `take_screenshot` - Capture screen
- `get_location` - GPS coordinates
- `access_calendar` - Read/write calendar events
- `scan_qr_code` - Use device camera
- `open_app` - Deep link to native app

**Multi-Viewer Challenge**:
In multi-viewer scenarios (user watching same chat on iPhone + iPad), we need coordination:
- **Primary Device Election**: Designate one device as executor
- **Failover**: If primary disconnects, promote secondary
- **Result Broadcasting**: All devices see execution results

**Design for Future Extensibility**:
```go
// Tool registry with executor type
type ToolExecutor string

const (
    ExecutorServer ToolExecutor = "server"  // Execute on proxy
    ExecutorClient ToolExecutor = "client"  // Execute on client device
)

type ToolDefinition struct {
    Name        string
    Executor    ToolExecutor
    Description string
    Handler     func(args map[string]interface{}) (interface{}, error) // Server tools only
}

var Registry = map[string]ToolDefinition{
    // Server-side tools (implemented now)
    "web_search":     {Executor: ExecutorServer, Handler: handleWebSearch},
    "get_weather":    {Executor: ExecutorServer, Handler: handleWeather},

    // Client-side tools (future - handler is nil)
    "read_file":      {Executor: ExecutorClient, Description: "Read local file"},
    "get_location":   {Executor: ExecutorClient, Description: "Get GPS coordinates"},
}
```

### Initial Implementation Scope

**Phase 1 (This Implementation)**: Server-Side Tools Only

We start with server-side tools because:
1. **No client coordination needed** - Simple, reliable execution
2. **Covers 80%+ of use cases** - Most tools are web APIs or stateless operations
3. **Better user experience** - Tools execute even if clients disconnect
4. **Easier to implement** - No multi-device coordination complexity
5. **We don't have local tools yet** - Current app doesn't require device access

**What We Build Now**:
- Tool registry infrastructure (extensible for future client tools)
- Server-side tool executor
- Tool result storage and broadcasting
- Integration with streaming architecture

**What We Defer**:
- Client tool coordination (primary/secondary election)
- Device capability detection
- Client-to-server tool result submission API
- Permission handling for device access

### Server-Side Tool Execution Flow

```
1. Client requests AI response (streaming)
   ↓
2. StreamSession reads from OpenAI/Claude
   ↓
3. AI responds with tool_calls in stream:
   data: {"choices":[{"delta":{"tool_calls":[
     {"id":"call_1","type":"function","function":{"name":"web_search","arguments":"..."}}
   ]}}]}
   ↓
4. StreamSession detects tool call:
   - Check Registry: is "web_search" a server tool? YES
   - Extract from stream
   ↓
5. Execute tool on server (background goroutine):
   result, err := ExecuteTool(toolCall)
   ↓
6. Store execution result:
   {
     "tool_call_id": "call_1",
     "result": {"search_results": [...]},
     "executed_by": "server",
     "executed_at": "2025-11-06T10:30:00Z"
   }
   ↓
7. Broadcast special event to all clients:
   event: tool_result
   data: {"tool_call_id":"call_1","result":{...}}
   ↓
8. When all tools complete, continue conversation:
   - Proxy sends tool results back to OpenAI
   - OpenAI generates final response
   - Stream continues to all clients
```

### Multi-Viewer Behavior with Server Tools

**Scenario**: User has iPhone + iPad watching same chat

```
AI: "I'll search the web for you"
tool_calls: [{id: "1", function: "web_search", args: "..."}]

✅ With Server-Side Execution:
- Proxy executes web_search ONCE
- Both iPhone and iPad receive:
  1. Tool call notification
  2. "Executing web_search..." status
  3. Tool result when complete
- No duplication, consistent results
```

### Tool Result Storage

Tool execution results are stored alongside the message:

```go
type ChatMessage struct {
    ID                  string
    EncryptedContent    string
    ToolCalls           []ToolCall           // AI's tool requests
    ToolResults         []ToolExecutionResult // Execution results
    ToolExecutionStatus string               // "pending", "executing", "completed", "failed"
    IsFromUser          bool
    ChatID              string
    Timestamp           interface{}
}

type ToolCall struct {
    ID       string
    Type     string // "function"
    Function ToolFunction
}

type ToolFunction struct {
    Name      string
    Arguments string // JSON string
}

type ToolExecutionResult struct {
    ToolCallID string
    Result     interface{} // JSON-serializable result
    Error      string      // Empty if successful
    ExecutedBy string      // "server" or client_id (future)
    ExecutedAt time.Time
}
```

### API Integration

**No client changes required** for server-side tools! Tools execute transparently:

1. Client sends standard request to `/chat/completions`
2. Proxy handles tool execution automatically
3. Client receives enhanced SSE stream with tool events:

```
data: {"choices":[{"delta":{"tool_calls":[...]}}]}

event: tool_executing
data: {"tool_call_id":"call_1","name":"web_search","status":"executing"}

event: tool_result
data: {"tool_call_id":"call_1","result":{...},"executed_by":"server"}

data: {"choices":[{"delta":{"content":"Based on the search..."}}]}
data: [DONE]
```

### Security Considerations

**Server-Side Tool Security**:
- **Sandboxing**: Code execution tools run in isolated containers
- **Rate Limiting**: Per-user quotas for expensive tools (API calls)
- **Credential Management**: API keys stored securely on server, never sent to clients
- **Input Validation**: Sanitize tool arguments before execution
- **Audit Logging**: All tool executions logged with user_id, timestamp, args

**Example Rate Limits**:
```go
var ToolLimits = map[string]RateLimit{
    "web_search":      {Requests: 100, Window: time.Hour},
    "image_generation": {Requests: 20, Window: time.Hour},
}
```

### Future: Client-Side Tool Extension Plan

When we need local tools, we'll extend with:

1. **Client Capabilities Header**:
   ```
   X-Client-Capabilities: ["read_file", "camera", "location"]
   ```

2. **Primary Device Coordination**:
   - First client to subscribe becomes "primary"
   - Primary receives tool assignment
   - Failover if primary disconnects

3. **Tool Result Submission Endpoint**:
   ```
   POST /api/v1/tool-results
   {
     "chat_id": "...",
     "message_id": "...",
     "tool_call_id": "...",
     "result": {...}
   }
   ```

4. **Enhanced SSE Events**:
   ```
   event: tool_assignment
   data: {"tool_call_id":"...","you_are_primary":true}

   event: tool_wait
   data: {"tool_call_id":"...","assigned_to":"client_456"}
   ```

**Architecture is ready** - just needs implementation when requirements emerge.

---

## Additional Architectural Improvements

Beyond streaming and tool execution, we'll implement two enhancements to simplify client integration and improve data consistency.

### 1. Automatic Model-to-Provider Routing

#### Current State

Clients must specify both model ID and provider URL:
```bash
POST /chat/completions
X-BASE-URL: https://api.openai.com/v1
{
  "model": "gpt-4",
  "messages": [...]
}
```

**Problems**:
- Clients need to maintain model-to-provider mapping
- Easy to misconfigure (e.g., send Claude model to OpenAI URL)
- Duplicate logic across iOS, Android, Web
- Hard to update when providers change endpoints

#### Proposed Solution

**Remove X-BASE-URL header requirement.** Proxy determines provider from model ID:

```go
// internal/routing/model_router.go
type ModelRouter struct {
    routes map[string]ProviderConfig
}

type ProviderConfig struct {
    BaseURL string
    APIKey  string
}

var DefaultRouter = &ModelRouter{
    routes: map[string]ProviderConfig{
        // OpenAI models
        "gpt-4":          {BaseURL: "https://api.openai.com/v1", APIKey: os.Getenv("OPENAI_API_KEY")},
        "gpt-4-turbo":    {BaseURL: "https://api.openai.com/v1", APIKey: os.Getenv("OPENAI_API_KEY")},
        "gpt-3.5-turbo":  {BaseURL: "https://api.openai.com/v1", APIKey: os.Getenv("OPENAI_API_KEY")},

        // Anthropic models
        "claude-3-opus":   {BaseURL: "https://api.anthropic.com", APIKey: os.Getenv("ANTHROPIC_API_KEY")},
        "claude-3-sonnet": {BaseURL: "https://api.anthropic.com", APIKey: os.Getenv("ANTHROPIC_API_KEY")},

        // OpenRouter (fallback for unknown models)
        "*": {BaseURL: "https://openrouter.ai/api/v1", APIKey: os.Getenv("OPENROUTER_API_KEY")},
    },
}

func (r *ModelRouter) GetProvider(modelID string) (*ProviderConfig, error) {
    // Exact match
    if config, exists := r.routes[modelID]; exists {
        return &config, nil
    }

    // Prefix match (e.g., "gpt-4-0125-preview" matches "gpt-4")
    for prefix, config := range r.routes {
        if strings.HasPrefix(modelID, prefix) {
            return &config, nil
        }
    }

    // Fallback to OpenRouter
    if fallback, exists := r.routes["*"]; exists {
        return &fallback, nil
    }

    return nil, fmt.Errorf("no provider configured for model: %s", modelID)
}
```

#### Updated Client Request

```bash
# NEW: No X-BASE-URL header needed!
POST /chat/completions
Authorization: Bearer <token>
X-Chat-ID: chat_abc
X-Message-ID: msg_123
{
  "model": "gpt-4",  # Proxy routes to OpenAI
  "messages": [...]
}

# OR
POST /chat/completions
{
  "model": "claude-3-sonnet",  # Proxy routes to Anthropic
  "messages": [...]
}

# OR unknown model
POST /chat/completions
{
  "model": "meta-llama/llama-3-70b",  # Proxy routes to OpenRouter
  "messages": [...]
}
```

#### Backward Compatibility

**X-BASE-URL still supported** for migration period:
- If `X-BASE-URL` provided: use it (legacy behavior)
- If `X-BASE-URL` missing: auto-route based on model (new behavior)

#### Benefits

✅ **Simpler client code** - No provider mapping needed
✅ **Centralized configuration** - Update routing in one place
✅ **Prevents misrouting** - Can't send wrong model to wrong provider
✅ **Easier model additions** - Add new models without client updates
✅ **Better error messages** - Proxy can validate model support

#### Configuration

**Environment Variables**:
```bash
# Provider API keys (already exist)
OPENAI_API_KEY=sk-...
ANTHROPIC_API_KEY=sk-ant-...
OPENROUTER_API_KEY=sk-or-...

# Optional: Custom routing overrides
MODEL_ROUTING_CONFIG=/path/to/routing.json
```

**Optional JSON Config** (`routing.json`):
```json
{
  "routes": {
    "gpt-4": {
      "base_url": "https://api.openai.com/v1",
      "api_key_env": "OPENAI_API_KEY"
    },
    "custom-model": {
      "base_url": "https://custom-provider.com/v1",
      "api_key_env": "CUSTOM_PROVIDER_KEY"
    }
  },
  "fallback": "openrouter"
}
```

---

### 2. Dual API Support (Chat Completions vs Responses API)

#### Overview

OpenAI and some providers support **two different API patterns** for chat interactions:

1. **Chat Completions API** (`/v1/chat/completions`) - Stateless, client manages conversation history
2. **Responses API** (`/v1/responses`) - Stateful, server manages conversation history (used by GPT-4.5+, GPT-5 Pro, etc.)

**Our StreamManager must support both** to ensure compatibility with all models.

#### API Comparison

| Feature | Chat Completions API | Responses API |
|---------|---------------------|---------------|
| **Endpoint** | `POST /v1/chat/completions` | `POST /v1/responses` |
| **State Management** | Client sends full message history | Server maintains state, uses `previous_response_id` |
| **Streaming** | SSE with `data:` events | SSE with `data:` events (same format) |
| **Tool Calling** | Tools in request, results in next request | Similar pattern but state managed server-side |
| **Resume/Continue** | Resend full history | Reference `response_id` |
| **Cancellation** | Close connection | Close connection + optional DELETE request |

#### How StreamManager Works with Both APIs

**Good News**: Our StreamManager architecture is **API-agnostic** at the streaming layer!

```
┌─────────────────────────────────────────────────┐
│              Client Request                      │
│  (Chat Completions OR Responses API)            │
└──────────────────┬──────────────────────────────┘
                   ↓
         ┌─────────────────────┐
         │   Proxy Handler     │
         │  (Route detection)  │
         └─────────┬───────────┘
                   ↓
    ┌──────────────┴──────────────┐
    │ Chat Completions?  │  Responses API?
    ↓                    ↓
┌────────────────┐  ┌────────────────┐
│ Route to:      │  │ Route to:      │
│ OpenAI/Anthropic│  │ OpenAI        │
│ /chat/completions│  │ /responses    │
└────────┬───────┘  └────────┬───────┘
         └──────────┬─────────┘
                    ↓
         ┌──────────────────────┐
         │  Upstream Provider   │
         │  (SSE stream back)   │
         └──────────┬───────────┘
                    ↓
         ┌──────────────────────┐
         │   StreamSession      │ ← Same broadcast logic!
         │  (read + broadcast)  │
         └──────────┬───────────┘
                    ↓
         ┌──────────────────────┐
         │  Multiple Clients    │
         └──────────────────────┘
```

**Key Insight**: StreamManager doesn't care about API type - it just:
1. Reads SSE stream from upstream (same format for both APIs)
2. Broadcasts chunks to subscribers
3. Stores complete response

#### Session Key Strategy

**For Chat Completions API**:
```go
sessionKey := chatID + ":" + messageID  // e.g., "chat_abc:msg_123"
```

**For Responses API**:
```go
// Option 1: Use responseId from OpenAI
sessionKey := "response:" + responseID  // e.g., "response:resp_xyz"

// Option 2: Still use chatID:messageID for consistency
sessionKey := chatID + ":" + messageID  // Client-provided IDs
```

**Recommendation**: Use `chatID:messageID` for both to maintain consistency. The `responseId` from OpenAI's Responses API is stored as metadata but not used as session key.

#### Routing Logic

```go
// internal/routing/model_router.go (enhanced)

type APIType string

const (
    APITypeChatCompletions APIType = "chat_completions"
    APITypeResponses       APIType = "responses"
)

type ProviderConfig struct {
    BaseURL string
    APIKey  string
    APIType APIType  // NEW: Determines which API to use
}

var DefaultRouter = &ModelRouter{
    routes: map[string]ProviderConfig{
        // Standard Chat Completions models
        "gpt-4":          {BaseURL: "https://api.openai.com/v1", APIType: APITypeChatCompletions},
        "gpt-4-turbo":    {BaseURL: "https://api.openai.com/v1", APIType: APITypeChatCompletions},
        "claude-3-sonnet": {BaseURL: "https://api.anthropic.com", APIType: APITypeChatCompletions},

        // Responses API models (GPT-4.5+, GPT-5 Pro)
        "gpt-4.5":        {BaseURL: "https://api.openai.com/v1", APIType: APITypeResponses},
        "gpt-5-pro":      {BaseURL: "https://api.openai.com/v1", APIType: APITypeResponses},

        // Fallback
        "*": {BaseURL: "https://openrouter.ai/api/v1", APIType: APITypeChatCompletions},
    },
}

func (r *ModelRouter) GetProvider(modelID string) (*ProviderConfig, error) {
    // Same matching logic, but now returns APIType too
}
```

#### Updated Proxy Handler

```go
// internal/proxy/handlers.go

func ProxyHandler(...) gin.HandlerFunc {
    return func(c *gin.Context) {
        model := extractModelFromRequestBody(c.Request.Body)

        // Get provider config (includes API type)
        providerConfig, err := modelRouter.GetProvider(model)
        if err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported model"})
            return
        }

        // Route based on API type
        var targetURL string
        switch providerConfig.APIType {
        case APITypeChatCompletions:
            targetURL = providerConfig.BaseURL + "/chat/completions"
        case APITypeResponses:
            targetURL = providerConfig.BaseURL + "/responses"
            // Handle response continuation if previous_response_id provided
            if prevResponseID := extractPreviousResponseID(c.Request.Body); prevResponseID != "" {
                targetURL = providerConfig.BaseURL + "/responses/" + prevResponseID
            }
        }

        // Continue with streaming (same StreamManager logic)
        session := streamManager.GetOrCreateSession(chatID, messageID, upstreamBody)
        subscriber := session.Subscribe(c.Request.Context(), subscriberID, replayFromStart)

        // Stream to client (identical for both APIs)
        streamToClient(c, subscriber)
    }
}
```

#### Client Request Examples

**Chat Completions API** (existing, most models):
```bash
POST /chat/completions
Authorization: Bearer <token>
X-Chat-ID: chat_abc
X-Message-ID: msg_123
{
  "model": "gpt-4",
  "messages": [
    {"role": "user", "content": "Hello"}
  ]
}
```

**Responses API** (GPT-5 Pro, GPT-4.5+):
```bash
# First message in conversation
POST /chat/completions  # Same endpoint! Proxy routes internally
Authorization: Bearer <token>
X-Chat-ID: chat_abc
X-Message-ID: msg_123
{
  "model": "gpt-5-pro",  # Proxy detects this uses Responses API
  "messages": [
    {"role": "user", "content": "Hello"}
  ]
}

# Continue conversation (with state)
POST /chat/completions
Authorization: Bearer <token>
X-Chat-ID: chat_abc
X-Message-ID: msg_456
X-Previous-Response-ID: resp_xyz  # NEW: Reference previous response
{
  "model": "gpt-5-pro",
  "messages": [
    {"role": "user", "content": "Tell me more"}
  ]
}
```

**Key Point**: Clients still use `/chat/completions` endpoint. The proxy handles the internal routing to `/responses` based on model configuration.

#### Special Considerations for Responses API

**1. State Persistence**:
- OpenAI Responses API maintains conversation state server-side
- Our StreamManager adds **broadcast capability** on top of OpenAI's state management
- We still store full messages in Firestore for our own records

**2. Response ID Tracking**:
```go
// StreamSession enhancement for Responses API
type StreamSession struct {
    ChatID    string
    MessageID string

    // NEW: For Responses API
    ResponseID       string  // From OpenAI's response
    PreviousResponseID string  // From request (for continuation)

    // ... existing fields
}

// Extract responseId from OpenAI response and store
func (s *StreamSession) extractResponseMetadata(chunk string) {
    if strings.Contains(chunk, `"id":"resp_`) {
        // Parse and store responseId for future continuations
        s.ResponseID = parseResponseID(chunk)
    }
}
```

**3. Stop/Cancel**:
- Chat Completions API: Just close connection
- Responses API: Close connection + optionally send `DELETE /responses/:responseId` to cancel server-side processing

```go
func (s *StreamSession) Stop(stoppedBy string) error {
    s.stopCancel()  // Cancel upstream read (both APIs)

    // If Responses API, send DELETE request
    if s.ResponseID != "" && s.apiType == APITypeResponses {
        go func() {
            req, _ := http.NewRequest("DELETE",
                baseURL+"/responses/"+s.ResponseID, nil)
            req.Header.Set("Authorization", "Bearer "+apiKey)
            http.DefaultClient.Do(req)
        }()
    }

    return nil
}
```

#### Benefits of Dual API Support

✅ **Future-proof** - Ready for GPT-5 Pro and future models using Responses API
✅ **Unified interface** - Clients don't need to know which API is used
✅ **Seamless migration** - Switch models without client changes
✅ **Best of both worlds** - OpenAI's state management + our broadcast capability

#### Testing Strategy

**Test both API types**:
```go
// Test Chat Completions API
func TestStreamManager_ChatCompletionsAPI(t *testing.T) {
    // Mock OpenAI /chat/completions endpoint
    // Verify streaming works
}

// Test Responses API
func TestStreamManager_ResponsesAPI(t *testing.T) {
    // Mock OpenAI /responses endpoint
    // Verify streaming works
    // Verify responseId extraction
    // Verify continuation with previous_response_id
}

// Test stop behavior
func TestStreamSession_Stop_ResponsesAPI(t *testing.T) {
    // Verify DELETE request sent to cancel server-side processing
}
```

---

### 3. Server-Side User Message Storage

#### Current State

Clients encrypt and store user messages to Firestore:
```typescript
// Client-side code (iOS/Android/Web)
const encryptedMessage = encrypt(userMessage, publicKey)
await firestore.collection('users/{userId}/chats/{chatId}/messages').add({
  content: encryptedMessage,
  isFromUser: true,
  timestamp: now()
})

// Then send to proxy for AI response
await fetch('/chat/completions', {
  body: JSON.stringify({messages: [...]})
})
```

**Problems**:
- Duplicate logic across all clients (iOS, Android, Web)
- Race conditions (Firestore write might fail, but AI request succeeds)
- Inconsistent storage (some clients may forget to store)
- Client failures leave gaps in chat history
- More client complexity

#### Proposed Solution

**Proxy stores user messages immediately upon receipt:**

```go
// In ProxyHandler, before forwarding to AI provider
func ProxyHandler(...) gin.HandlerFunc {
    return func(c *gin.Context) {
        // 1. Extract user message from request
        requestBody, _ := io.ReadAll(c.Request.Body)
        userMessage := extractUserMessage(requestBody)

        chatID := c.GetHeader("X-Chat-ID")
        messageID := c.GetHeader("X-Message-ID") // User message ID
        userID, _ := auth.GetUserID(c)

        // 2. Store user message to Firestore (async)
        if messageService != nil && chatID != "" && messageID != "" {
            encryptionEnabled := c.GetHeader("X-Encryption-Enabled") == "true"

            go messageService.SaveUserMessage(context.Background(), messaging.UserMessage{
                UserID:            userID,
                ChatID:            chatID,
                MessageID:         messageID,
                Content:           userMessage,
                EncryptionEnabled: encryptionEnabled,
                Timestamp:         time.Now(),
            })
        }

        // 3. Continue with AI request (existing flow)
        proxy.ServeHTTP(c.Writer, c.Request)
    }
}
```

#### Message Service Enhancement

```go
// internal/messaging/service.go
type UserMessage struct {
    UserID            string
    ChatID            string
    MessageID         string
    Content           string
    EncryptionEnabled bool
    Timestamp         time.Time
}

func (s *Service) SaveUserMessage(ctx context.Context, msg UserMessage) error {
    var encryptedContent string
    var publicKeyUsed string

    if msg.EncryptionEnabled {
        // Fetch user's public key
        publicKey, err := s.getPublicKey(ctx, msg.UserID)
        if err != nil {
            log.Error("Failed to fetch public key", "error", err)
            // Graceful degradation: store as plaintext
            encryptedContent = msg.Content
            publicKeyUsed = "none"
        } else {
            // Encrypt user message with user's public key
            encrypted, err := s.encryptionService.EncryptMessage(msg.Content, publicKey.Public)
            if err != nil {
                log.Error("Encryption failed", "error", err)
                encryptedContent = msg.Content
                publicKeyUsed = "none"
            } else {
                encryptedContent = encrypted
                publicKeyUsed = publicKey.Public
            }
        }
    } else {
        // Encryption not enabled, store as-is
        encryptedContent = msg.Content
        publicKeyUsed = "none"
    }

    // Save to Firestore
    chatMsg := &ChatMessage{
        ID:                  msg.MessageID,
        EncryptedContent:    encryptedContent,
        PublicEncryptionKey: publicKeyUsed,
        IsFromUser:          true,
        ChatID:              msg.ChatID,
        Timestamp:           firestore.ServerTimestamp,
    }

    return s.firestoreClient.SaveMessage(ctx, msg.UserID, chatMsg)
}
```

#### Updated Client Flow

```bash
# NEW: Client just sends message, proxy handles storage
POST /chat/completions
Authorization: Bearer <token>
X-Chat-ID: chat_abc
X-Message-ID: msg_user_123          # User message ID
X-Encryption-Enabled: true          # Proxy encrypts if true
{
  "model": "gpt-4",
  "messages": [
    {"role": "user", "content": "Hello, how are you?"}
  ]
}

# Proxy:
# 1. Immediately stores user message to Firestore (encrypted if header=true)
# 2. Forwards request to OpenAI or other inference endpoint
# 3. Stores AI response when complete (as before)

# Result: Both user and AI messages in Firestore, atomically
```

#### Benefits

✅ **Simplified clients** - No Firestore SDK needed for message storage
✅ **Atomic storage** - User message and AI response stored together
✅ **Consistent encryption** - Same encryption logic for all messages
✅ **Better reliability** - Proxy ensures storage even if client crashes
✅ **Easier debugging** - All storage logic in one place
✅ **Reduced client complexity** - Clients just send HTTP requests

#### Backward Compatibility

**Clients can still store messages directly to Firestore:**
- If `X-Message-ID` missing: Proxy doesn't store user message (legacy behavior)
- If `X-Message-ID` present: Proxy stores user message (new behavior)
- Clients can gradually migrate without breaking changes

#### Security Considerations

**Encryption Flow**:
1. Client sends plaintext message (already over TLS)
2. Proxy encrypts with user's public key (same as current AI response encryption)
3. Encrypted message stored in Firestore
4. Client retrieves and decrypts with private key (unchanged)

**No additional security risk** - same encryption as current AI responses.

---

## Implementation Phases

### Phase 1: Core Stream Manager (Week 1)
**Goal**: Implement broadcast infrastructure without changing existing handlers

**Deliverables**:
- `internal/streaming/manager.go` - StreamManager + StreamSession
- `internal/streaming/subscriber.go` - Subscriber abstraction
- `internal/streaming/types.go` - Shared types
- Unit tests for broadcast logic

**Success Criteria**:
- StreamSession can fanout to 10+ subscribers
- Upstream reading continues if all subscribers disconnect
- Memory cleanup works correctly

**Risk**: Low (no changes to production code paths yet)

---

### Phase 2: Proxy Handler Integration + Routing + User Message Storage (Week 2-3)
**Goal**: Update proxy handlers to use StreamManager, add model routing, and user message storage

**Changes**:
1. Modify `ProxyHandler()` to check for active streams
2. Update `handleStreamingResponse()` to use StreamSession
3. Add `chatID` + `messageID` extraction from headers/request
4. **Add model-to-provider routing** (remove X-BASE-URL requirement)
5. **Add user message storage** (proxy stores user messages to Firestore)
6. Maintain backward compatibility (if headers missing, use old path)

**Deliverables**:
- Updated `internal/proxy/handlers.go` - streaming + user message storage
- `internal/routing/model_router.go` - model-to-provider routing
- `internal/messaging/service.go` - SaveUserMessage method
- Feature flag: `ENABLE_STREAM_BROADCAST` (default: false)
- Configuration: default routing table + optional JSON config
- Integration tests (streaming, routing, user message storage)

**Success Criteria**:
- Single client requests work identically to before
- Multi-client requests share streams
- Client disconnect doesn't stop upstream reading
- Model routing works for OpenAI, Anthropic, OpenRouter
- Unknown models fallback to OpenRouter
- X-BASE-URL still works (backward compatible)
- User messages stored atomically with AI responses
- Encryption works for user messages

**Risk**: Medium (changes core proxy logic, but feature-flagged and backward compatible)

---

### Phase 3: Client Subscription API + Stop Control (Week 4)
**Goal**: Add REST endpoints for joining streams and stopping generation

**New Endpoints**:
```
GET  /api/v1/chats/:chatId/messages/:messageId/stream
POST /api/v1/chats/:chatId/messages/:messageId/stop
```

**Deliverables**:
- Handler for subscription endpoint (join active/completed streams)
- Handler for stop endpoint (cancel in-progress generation)
- StreamSession.Stop() method with context cancellation
- Tool execution cancellation on stop
- Partial message storage with stopped flag
- Authentication + authorization (user must own chat)
- Documentation for client integration

**Success Criteria**:
- Client can join in-progress stream
- Client receives full response from beginning (replay)
- Late-join doesn't affect other clients
- User can stop generation at any time
- All watching clients receive stop notification
- Partial responses stored with stopped=true flag
- Running tools cancelled on stop
- Upstream connection terminated cleanly

**Risk**: Low (additive changes, doesn't break existing flows)

---

### Phase 4: Enhanced Message Storage (Week 5)
**Goal**: Ensure message storage always completes

**Changes**:
1. Move message extraction to StreamSession completion
2. Extract tool calls from buffered chunks
3. Handle encryption for stored messages
4. Add retry logic for Firestore failures

**Deliverables**:
- Updated `internal/messaging/service.go`
- Tool call extraction utilities
- Observability (metrics, logs)

**Success Criteria**:
- 100% message completion rate
- All tool calls captured correctly
- Failed Firestore writes are retried

**Risk**: Low (improves existing functionality)

---

### Phase 5: Server-Side Tool Execution (Week 6-7)
**Goal**: Implement server-side tool execution infrastructure

**Changes**:
1. Create tool registry with extensible architecture
2. Implement tool executor for server-side tools
3. Add tool call detection in streaming
4. Implement tool result storage and broadcasting
5. Add first server-side tools (web_search, etc.)

**Deliverables**:
- `internal/tools/registry.go` - Tool registry with ExecutorServer/ExecutorClient types
- `internal/tools/executor.go` - Server-side tool execution engine
- `internal/tools/handlers.go` - Individual tool implementations
- `internal/streaming/tools.go` - Tool call detection and coordination in streams
- Updated message storage schema with ToolCalls and ToolResults
- Integration tests for tool execution
- Documentation for adding new tools

**Initial Server-Side Tools**:
- `web_search` - SerpAPI/Exa integration (reuse existing search service)
- `calculate` - Safe mathematical expression evaluation
- `get_time` - Current time/date information

**Success Criteria**:
- Tool calls detected in streaming responses
- Server tools execute exactly once per call
- Tool results broadcast to all watching clients
- Tool execution continues even if clients disconnect
- Tool results stored in Firestore with message
- All clients see consistent tool results

**Architecture Decisions**:
- Registry pattern allows easy addition of new tools
- Executor type (server/client) built into registry for future extensibility
- Client-side tools can be added later without breaking changes
- Tool execution isolated from streaming logic (separation of concerns)

**Risk**: Medium (new feature, but isolated from core streaming)

---

### Phase 6: Observability & Production Hardening (Week 8)
**Goal**: Make production-ready with monitoring

**Deliverables**:
- Prometheus metrics (active streams, subscriber count, chunk rates)
- Health check endpoint showing stream status
- Admin API to inspect/terminate streams
- Load testing results (100 concurrent streams)
- Runbook documentation

**Success Criteria**:
- Can monitor stream health in production
- Can diagnose issues via metrics/logs
- System handles peak load gracefully

**Risk**: Low (observability additions)

---

## Detailed Component Design

### 1. StreamManager (`internal/streaming/manager.go`)

**Responsibilities**:
- Manage lifecycle of all active StreamSessions
- Provide session lookup by `(chatID, messageID)`
- Cleanup expired sessions
- Provide metrics and observability

**Interface**:
```go
type StreamManager struct {
    logger   *logger.Logger
    sessions map[string]*StreamSession  // key: "chatID:messageID"
    mu       sync.RWMutex
    metrics  *StreamMetrics
}

// GetOrCreateSession finds existing session or creates new one
// If session exists: returns it for subscription
// If not: creates new session and starts upstream read
func (sm *StreamManager) GetOrCreateSession(
    chatID, messageID string,
    upstreamBody io.ReadCloser,
) *StreamSession

// GetSession retrieves active session (nil if not found)
func (sm *StreamManager) GetSession(chatID, messageID string) *StreamSession

// CleanupExpiredSessions removes completed sessions older than TTL
func (sm *StreamManager) CleanupExpiredSessions(ttl time.Duration) int

// GetActiveStreams returns list of active session IDs
func (sm *StreamManager) GetActiveStreams() []StreamInfo

// GetMetrics returns current streaming metrics
func (sm *StreamManager) GetMetrics() StreamMetrics
```

**Concurrency Safety**:
- `sync.RWMutex` protects sessions map
- Read-heavy workload (many lookups, few creates) → RWMutex appropriate
- Each StreamSession manages its own subscriber concurrency

**Memory Management**:
- Sessions auto-cleanup after 30min of completion
- Background goroutine runs cleanup every 5min
- Max chunk buffer size: 10,000 chunks (~10MB worst case per stream)

---

### 2. StreamSession (`internal/streaming/session.go`)

**Responsibilities**:
- Read from upstream AI provider (OpenAI/Claude)
- Buffer all chunks for late-join replay
- Broadcast chunks to subscribers
- Extract and store complete message when done

**Interface**:
```go
type StreamSession struct {
    ChatID    string
    MessageID string
    StartTime time.Time

    // Upstream reading (private goroutine)
    upstreamBody io.ReadCloser
    completed    bool
    err          error

    // Stop control
    stopCtx       context.Context
    stopCancel    context.CancelFunc
    stopped       bool
    stoppedBy     string    // User ID who stopped, or "system_timeout"
    stoppedAt     time.Time
    stopMu        sync.RWMutex

    // Chunk storage
    chunks       []StreamChunk
    chunksMu     sync.RWMutex

    // Subscriber management
    subscribers   map[string]*StreamSubscriber
    subscribersMu sync.RWMutex

    // Tool execution tracking
    runningToolExecutions map[string]*ToolExecutionContext
    toolExecutionsMu      sync.RWMutex

    logger *logger.Logger
}

// Subscribe adds a new client to receive chunks
// replayFromStart: if true, sends all buffered chunks before live chunks
func (s *StreamSession) Subscribe(
    ctx context.Context,
    subscriberID string,
    replayFromStart bool,
) *StreamSubscriber

// Unsubscribe removes a client
func (s *StreamSession) Unsubscribe(subscriberID string)

// Stop cancels the upstream read and broadcasts stop event to all clients
// stoppedBy: user ID who requested stop, or "system_timeout" for automatic stops
func (s *StreamSession) Stop(stoppedBy string) error

// GetStoredChunks returns all chunks buffered so far
func (s *StreamSession) GetStoredChunks() []StreamChunk

// IsCompleted returns whether upstream read finished
func (s *StreamSession) IsCompleted() bool

// IsStopped returns whether the stream was stopped by user/system
func (s *StreamSession) IsStopped() bool

// GetContent extracts full message content from chunks
func (s *StreamSession) GetContent() string

// GetToolCalls extracts tool calls from chunks
func (s *StreamSession) GetToolCalls() []ToolCall
```

**Lifecycle**:
```
1. Created by StreamManager when first client requests
2. Starts background goroutine: readUpstream()
3. Accepts subscriber subscriptions (concurrent)
4. Broadcasts chunks to all subscribers (non-blocking)
5. On completion:
   - Extract content + tool calls
   - Save to Firestore
   - Close all subscriber channels
   - Mark as completed
6. Kept alive for 30min (late joiners can still subscribe)
7. Cleaned up by StreamManager
```

**Error Handling**:
- Upstream read errors stored in `session.err`
- Broadcast continues with error chunk (clients see error)
- Firestore save failure logged but doesn't fail stream
- Panic recovery in readUpstream goroutine

---

### 3. StreamSubscriber (`internal/streaming/subscriber.go`)

**Responsibilities**:
- Represent one client's subscription to a stream
- Buffered channel for receiving chunks
- Context for cancellation

**Interface**:
```go
type StreamSubscriber struct {
    ID       string
    Ch       chan StreamChunk  // Buffered channel (100 capacity)
    JoinedAt time.Time

    ctx    context.Context
    cancel context.CancelFunc
}

// Close closes the subscriber channel and cancels context
func (s *StreamSubscriber) Close()
```

**Design Choices**:
- **Buffered channel (100 capacity)**: Handles burst traffic, smooth over network jitter
- **Context for cancellation**: Clean shutdown when client disconnects
- **Non-blocking sends**: Slow subscriber doesn't block others

---

### 4. StreamChunk (`internal/streaming/types.go`)

**Purpose**: Represent one SSE line from AI provider

```go
type StreamChunk struct {
    Index     int           // Sequential index (0, 1, 2, ...)
    Line      string        // Raw SSE line (e.g., "data: {...}")
    Timestamp time.Time     // When chunk was received
    IsFinal   bool          // true for last chunk
}
```

**Why store raw lines instead of parsed JSON?**
- **Simplicity**: No need to understand every provider's format
- **Flexibility**: Works with OpenAI, Anthropic, OpenRouter without changes
- **Debugging**: Can inspect exact bytes from upstream
- **Forward compatibility**: New fields from providers don't break us

**Parsing happens at two points**:
1. In ProxyHandler for real-time client streaming (unchanged)
2. In StreamSession.GetContent/GetToolCalls for storage

---

## API Design

### 1. Existing Endpoint Changes

#### `POST /chat/completions` (Modified)

**Request Headers** (new, optional):
```
X-Chat-ID: <chatID>              # Required for multi-viewer support
X-Message-ID: <messageID>        # Required for multi-viewer support (AI response ID)
X-User-Message-ID: <userMsgID>   # Optional: User message ID for storage
X-Encryption-Enabled: true       # Optional: Encrypt messages in Firestore
```

**Removed Headers**:
```
X-BASE-URL: <url>  # NO LONGER NEEDED! Proxy auto-routes based on model
```

**Behavior Changes**:
- **Model routing**: Proxy automatically routes to correct provider (OpenAI, Anthropic, OpenRouter) based on model ID
- **User message storage**: If `X-User-Message-ID` present, proxy stores user message to Firestore
- **Stream broadcast**: If `X-Chat-ID` + `X-Message-ID` present, check for active stream and subscribe if exists
- **Backward compatible**: X-BASE-URL still works if provided (legacy)

**Response**: Identical to current (SSE stream)

**Example Client Flow**:
```bash
# Client A starts generation (NEW SIMPLIFIED FLOW)
curl -X POST http://proxy/chat/completions \
  -H "Authorization: Bearer <token>" \
  -H "X-Chat-ID: chat_abc" \
  -H "X-Message-ID: msg_ai_123" \
  -H "X-User-Message-ID: msg_user_123" \
  -H "X-Encryption-Enabled: true" \
  -d '{"model": "gpt-4", "messages": [{"role":"user","content":"Hello"}], "stream": true}'

# Proxy:
# 1. Routes to OpenAI based on "gpt-4" model
# 2. Stores user message to Firestore (encrypted)
# 3. Creates StreamSession for AI response
# 4. Streams response to client
# 5. Stores AI response to Firestore when complete

# Client B joins same stream (concurrent) - watching on different device
curl -X POST http://proxy/chat/completions \
  -H "Authorization: Bearer <token>" \
  -H "X-Chat-ID: chat_abc" \
  -H "X-Message-ID: msg_ai_123" \  # Same AI message ID!
  -d '{"model": "gpt-4", "messages": [{"role":"user","content":"Hello"}], "stream": true}'

# Proxy:
# 1. Detects existing StreamSession for msg_ai_123
# 2. Subscribes Client B to existing stream
# 3. Both clients receive identical stream
# 4. No duplicate OpenAI request

# OR use different model (Anthropic)
curl -X POST http://proxy/chat/completions \
  -H "Authorization: Bearer <token>" \
  -H "X-Chat-ID: chat_xyz" \
  -H "X-Message-ID: msg_456" \
  -d '{"model": "claude-3-sonnet", "messages": [...]}'

# Proxy automatically routes to Anthropic API
```

---

### 2. New Endpoints

#### `GET /api/v1/chats/:chatId/messages/:messageId/stream`

**Purpose**: Subscribe to active or completed stream

**Auth**: Requires Bearer token, user must own chat

**Query Parameters**:
- `replay=true` (default: true) - Replay from beginning if joining mid-stream
- `wait_for_completion=true` (default: false) - If stream not found, wait up to 30s for it to start

**Response**: SSE stream

**Status Codes**:
- `200 OK` - Stream active or completed, data returned
- `404 Not Found` - Stream not found and wait_for_completion=false
- `403 Forbidden` - User doesn't own chat
- `410 Gone` - Stream completed and cleaned up (>30min ago)

**Example**:
```bash
# Late joiner (stream already in progress)
curl -X GET http://proxy/api/v1/chats/chat_abc/messages/msg_123/stream \
  -H "Authorization: Bearer <token>" \
  -H "Accept: text/event-stream"

# Response: Full stream from beginning (replay=true)
data: {"choices":[{"delta":{"role":"assistant"},...}]}
data: {"choices":[{"delta":{"content":"Hello"},...}]}
...
data: [DONE]
```

---

#### `POST /api/v1/chats/:chatId/messages/:messageId/stop`

**Purpose**: Stop/cancel an in-progress AI response generation

**Auth**: Requires Bearer token, user must own chat

**Request Body**: None

**Response**:
```json
{
  "stopped": true,
  "message_id": "msg_123",
  "chunks_generated": 145,
  "stopped_at": "2025-11-06T10:35:22Z",
  "partial_content_stored": true
}
```

**Status Codes**:
- `200 OK` - Stream stopped successfully
- `404 Not Found` - Stream not found (already completed or doesn't exist)
- `403 Forbidden` - User doesn't own chat
- `409 Conflict` - Stream already completed naturally

**Behavior**:
- **Cancels upstream request**: Stops reading from OpenAI/Claude immediately
- **Broadcasts stop event**: All subscribed clients receive "stopped" event
- **Stores partial response**: Saves generated content so far with `stopped: true` flag
- **Cancels running tools**: Any server-side tools executing are cancelled
- **Cleanup**: Session marked as completed, cleanup timer starts (30min)

**Multi-Viewer Behavior**:
- Any authenticated user who owns the chat can stop the generation
- When stopped, **all clients** watching the stream receive stop notification
- Prevents one client from continuing while others think it's stopped

**Example**:
```bash
# User decides to stop generation mid-stream
curl -X POST http://proxy/api/v1/chats/chat_abc/messages/msg_123/stop \
  -H "Authorization: Bearer <token>"

# Response
{
  "stopped": true,
  "message_id": "msg_123",
  "chunks_generated": 145,
  "stopped_at": "2025-11-06T10:35:22Z",
  "partial_content_stored": true
}

# All clients watching this stream receive SSE event:
event: stream_stopped
data: {"message_id": "msg_123", "stopped_by": "user", "reason": "user_cancelled"}

# Then connection closes with:
data: [DONE]
```

**Stop Event Format**:
Clients receive a special SSE event when stream is stopped:
```
event: stream_stopped
data: {
  "message_id": "msg_123",
  "stopped_by": "user",
  "reason": "user_cancelled",
  "chunks_generated": 145,
  "partial_content_available": true
}

data: [DONE]
```

---

## Edge Cases & Error Handling

### Edge Case 1: All Clients Disconnect During Stream

**Scenario**:
```
1. Client A starts request → StreamSession created, upstream read starts
2. Client A subscribes, receives chunks
3. Client A disconnects (e.g., browser tab closed)
4. No other clients subscribed
```

**Handling**:
```go
// In StreamSession.readUpstream()
for scanner.Scan() {
    // NO check for subscriber count
    // Continue reading regardless

    chunk := parseChunk(scanner.Text())
    s.storeChunk(chunk)  // Always store

    s.broadcast(chunk)  // Broadcast (no-op if no subscribers)
}

// After loop completes
s.extractAndSaveMessage()  // Always save to Firestore
```

**Outcome**: ✅ Full response stored in Firestore even though no clients received it

---

### Edge Case 2: Concurrent Requests for Same Message

**Scenario**:
```
1. Client A sends POST /chat/completions with chat_abc:msg_123
2. Client B sends POST /chat/completions with chat_abc:msg_123 (100ms later)
3. Both should receive same stream
```

**Handling**:
```go
// In ProxyHandler
sessionKey := chatID + ":" + messageID

// Thread-safe lookup
streamManager.mu.RLock()
session, exists := streamManager.sessions[sessionKey]
streamManager.mu.RUnlock()

if exists {
    // Subscribe to existing session
    subscriber := session.Subscribe(ctx, subscriberID, replayFromStart=true)
    streamToClient(subscriber.Ch)
    return
}

// Create new session (with double-check locking)
streamManager.mu.Lock()
if session, exists := streamManager.sessions[sessionKey]; exists {
    // Another goroutine created it
    streamManager.mu.Unlock()
    subscriber := session.Subscribe(ctx, subscriberID, replayFromStart=true)
    streamToClient(subscriber.Ch)
    return
}

// We create it
session = NewStreamSession(chatID, messageID, upstreamBody)
streamManager.sessions[sessionKey] = session
streamManager.mu.Unlock()

go session.readUpstream()  // Start reading
subscriber := session.Subscribe(ctx, subscriberID, replayFromStart=false)
streamToClient(subscriber.Ch)
```

**Outcome**: ✅ Only one upstream request to OpenAI, both clients receive stream

---

### Edge Case 3: Upstream Provider Timeout

**Scenario**:
```
1. Client requests, StreamSession created
2. OpenAI (or other inference provider) takes 3 minutes to respond (hung connection)
3. Multiple clients waiting
```

**Handling**:
```go
// In StreamSession creation
ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
defer cancel()

// Use timeout context for upstream read
req, _ := http.NewRequestWithContext(ctx, ...)
resp, err := client.Do(req)

// In readUpstream()
for scanner.Scan() {
    select {
    case <-ctx.Done():
        // Timeout reached
        s.err = errors.New("upstream timeout after 3 minutes")
        s.broadcastError("Stream timeout")
        return
    default:
    }
    // ... normal processing
}
```

**Outcome**: ✅ Clients receive error after 10min, session cleaned up

---

### Edge Case 4: Client Subscribes After Stream Completion

**Scenario**:
```
1. Client A requests, gets full response, stream completes
2. Client B tries to subscribe 10 minutes later
3. Session still in memory (30min TTL)
```

**Handling**:
```go
// In StreamSession.Subscribe()
if s.IsCompleted() {
    // Send all buffered chunks immediately
    go func() {
        for _, chunk := range s.GetStoredChunks() {
            subscriber.Ch <- chunk
        }
        close(subscriber.Ch)
    }()
    return subscriber
}

// Normal subscription for in-progress stream
```

**Outcome**: ✅ Client B receives full response instantly (from buffer)

---

### Edge Case 5: Malformed Upstream Response

**Scenario**:
```
1. OpenAI sends invalid JSON or truncated response
2. Scanner encounters error
```

**Handling**:
```go
// In readUpstream()
if err := scanner.Err(); err != nil {
    s.logger.Error("scanner error", slog.String("error", err.Error()))
    s.err = err

    // Broadcast error chunk
    errorChunk := StreamChunk{
        Index: len(s.chunks),
        Line: "data: {\"error\": \"upstream_error\"}",
        IsFinal: true,
    }
    s.storeChunk(errorChunk)
    s.broadcast(errorChunk)
}

// Still save partial response to Firestore (with error flag)
s.saveMessage(isError=true)
```

**Outcome**: ✅ Clients receive error, partial response stored for debugging

---

### Edge Case 6: Memory Pressure from Large Response

**Scenario**:
```
1. GPT-4 generates 10,000 token response (40KB+ SSE data)
2. Chunk buffer grows large
3. Multiple concurrent streams
```

**Handling**:
```go
// In StreamSession
const maxChunks = 10000  // ~10MB worst case
const maxChunkSize = 1024 * 1024  // 1MB per chunk

func (s *StreamSession) storeChunk(chunk StreamChunk) {
    s.chunksMu.Lock()
    defer s.chunksMu.Unlock()

    // Safety limits
    if len(s.chunks) >= maxChunks {
        s.logger.Warn("chunk buffer full, dropping old chunks")
        // Keep first 100 and last 9900 chunks
        s.chunks = append(s.chunks[:100], s.chunks[len(s.chunks)-9900:]...)
    }

    if len(chunk.Line) > maxChunkSize {
        s.logger.Error("chunk too large, truncating")
        chunk.Line = chunk.Line[:maxChunkSize]
    }

    s.chunks = append(s.chunks, chunk)
}

// In StreamManager.CleanupExpiredSessions()
// Aggressive cleanup under memory pressure
if runtime.MemStats().Alloc > 500*1024*1024 {  // >500MB
    ttl = 1 * time.Minute  // Reduce TTL
}
```

**Outcome**: ✅ Memory bounded, old chunks dropped if needed

---

### Edge Case 7: User Stops Generation Mid-Stream

**Scenario**:
```
1. Client A requests AI response, stream starts
2. OpenAI generating response, 50% complete
3. User clicks "Stop" button in UI
4. Client B is also watching the same stream
5. Client A sends stop request
```

**Handling**:
```go
// In StreamSession
type StreamSession struct {
    // ... existing fields
    stopCtx       context.Context
    stopCancel    context.CancelFunc
    stopped       bool
    stoppedBy     string
    stoppedAt     time.Time
    mu            sync.RWMutex
}

// Stop method cancels upstream reading
func (s *StreamSession) Stop(stoppedBy string) error {
    s.mu.Lock()
    defer s.mu.Unlock()

    if s.completed {
        return errors.New("stream already completed")
    }

    if s.stopped {
        return errors.New("stream already stopped")
    }

    s.stopped = true
    s.stoppedBy = stoppedBy
    s.stoppedAt = time.Now()

    // Cancel upstream context - stops reading from OpenAI
    s.stopCancel()

    // Broadcast stop event to all subscribers
    stopEvent := StreamChunk{
        Index:     len(s.chunks),
        Line:      fmt.Sprintf("event: stream_stopped\ndata: {\"message_id\":\"%s\",\"stopped_by\":\"%s\",\"reason\":\"user_cancelled\"}", s.MessageID, stoppedBy),
        Timestamp: time.Now(),
        IsFinal:   true,
    }
    s.broadcast(stopEvent)

    s.logger.Info("stream stopped by user",
        slog.String("chat_id", s.ChatID),
        slog.String("message_id", s.MessageID),
        slog.String("stopped_by", stoppedBy),
        slog.Int("chunks_generated", len(s.chunks)))

    return nil
}

// In readUpstream() - use stopCtx instead of background context
func (s *StreamSession) readUpstream() {
    defer s.markCompleted()

    for scanner.Scan() {
        select {
        case <-s.stopCtx.Done():
            // User stopped or timeout
            s.logger.Info("upstream read cancelled",
                slog.String("reason", s.stopCtx.Err().Error()))

            // Save partial response
            s.savePartialMessage(stopped=true)
            return
        default:
        }

        // Normal processing
        line := scanner.Text()
        chunk := StreamChunk{...}
        s.storeChunk(chunk)
        s.broadcast(chunk)
    }

    // Natural completion
    s.saveCompleteMessage()
}

// Stop endpoint handler
func StopStreamHandler(streamManager *StreamManager) gin.HandlerFunc {
    return func(c *gin.Context) {
        chatID := c.Param("chatId")
        messageID := c.Param("messageId")
        userID, _ := auth.GetUserID(c)

        // Get session
        session := streamManager.GetSession(chatID, messageID)
        if session == nil {
            c.JSON(http.StatusNotFound, gin.H{"error": "Stream not found"})
            return
        }

        // Stop the session
        err := session.Stop(userID)
        if err != nil {
            if strings.Contains(err.Error(), "already completed") {
                c.JSON(http.StatusConflict, gin.H{"error": "Stream already completed"})
                return
            }
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }

        c.JSON(http.StatusOK, gin.H{
            "stopped":                true,
            "message_id":             messageID,
            "chunks_generated":       len(session.GetStoredChunks()),
            "stopped_at":             session.stoppedAt,
            "partial_content_stored": true,
        })
    }
}
```

**Multi-Viewer Coordination**:
```
Scenario: Client A (iPhone) and Client B (iPad) both watching same stream

1. User stops on iPhone (Client A)
   → POST /api/v1/chats/chat_abc/messages/msg_123/stop

2. Proxy:
   → Cancels upstream OpenAI request
   → Broadcasts "stream_stopped" event to ALL subscribers

3. Both iPhone (Client A) and iPad (Client B) receive:
   event: stream_stopped
   data: {...}

4. Both clients:
   → Display "Generation stopped" UI
   → Stop showing loading indicators
   → Show partial content

Result: ✅ Consistent state across all devices
```

**Tool Execution Handling**:
```go
// If server-side tools are running when stop is requested

type ToolExecutionContext struct {
    ctx    context.Context
    cancel context.CancelFunc
}

// In StreamSession.Stop()
func (s *StreamSession) Stop(stoppedBy string) error {
    // ... existing stop logic

    // Cancel all running tool executions
    s.toolExecutionsMu.Lock()
    for toolCallID, execCtx := range s.runningToolExecutions {
        execCtx.cancel()
        s.logger.Info("cancelled tool execution",
            slog.String("tool_call_id", toolCallID))
    }
    s.toolExecutionsMu.Unlock()

    return nil
}
```

**Message Storage**:
Partial responses are stored with metadata:
```go
type ChatMessage struct {
    ID                  string
    EncryptedContent    string
    Stopped             bool      // NEW: true if user stopped
    StoppedBy           string    // NEW: user ID who stopped
    StoppedAt           time.Time // NEW: when stopped
    CompletionTokens    int       // May be less than expected
    IsFromUser          bool
    ChatID              string
    Timestamp           interface{}
}

// Clients can distinguish between:
// 1. Complete response: Stopped = false
// 2. User-stopped response: Stopped = true, StoppedBy = "user_123"
// 3. Timeout-stopped response: Stopped = true, StoppedBy = "system_timeout"
```

**Outcome**:
- ✅ User can stop generation at any time
- ✅ All watching clients notified immediately
- ✅ Partial content saved for reference
- ✅ Resources cleaned up (upstream connection, tool executions)
- ✅ Consistent multi-viewer state

---

## Testing Strategy

### Unit Tests

**StreamManager Tests** (`internal/streaming/manager_test.go`):
```go
func TestStreamManager_GetOrCreateSession(t *testing.T)
func TestStreamManager_ConcurrentSessionCreation(t *testing.T)
func TestStreamManager_SessionCleanup(t *testing.T)
func TestStreamManager_GetActiveStreams(t *testing.T)
```

**StreamSession Tests** (`internal/streaming/session_test.go`):
```go
func TestStreamSession_SingleSubscriber(t *testing.T)
func TestStreamSession_MultipleSubscribers(t *testing.T)
func TestStreamSession_LateJoiner(t *testing.T)
func TestStreamSession_AllClientsDisconnect(t *testing.T)
func TestStreamSession_SlowSubscriber(t *testing.T)
func TestStreamSession_UpstreamError(t *testing.T)
func TestStreamSession_ContentExtraction(t *testing.T)
func TestStreamSession_ToolCallExtraction(t *testing.T)
func TestStreamSession_UserStop(t *testing.T)
func TestStreamSession_StopMultipleSubscribers(t *testing.T)
func TestStreamSession_StopWithRunningTools(t *testing.T)
func TestStreamSession_StopAlreadyCompleted(t *testing.T)
```

**Coverage Target**: >80% for streaming package

---

### Integration Tests

**Proxy Handler Tests** (`internal/proxy/handlers_test.go`):
```go
func TestProxyHandler_StreamBroadcast(t *testing.T) {
    // Setup: Mock OpenAI returning 100 chunks
    // Action: Two clients request with same chat_id:message_id
    // Assert: Both receive identical 100 chunks
    // Assert: Only one upstream request to OpenAI
}

func TestProxyHandler_ClientDisconnect(t *testing.T) {
    // Setup: Mock OpenAI returning 100 chunks slowly
    // Action: Client subscribes, disconnects at chunk 50
    // Assert: Upstream reading continues to chunk 100
    // Assert: Message stored in Firestore with full content
}

func TestProxyHandler_LateJoiner(t *testing.T) {
    // Setup: Stream in progress at chunk 50
    // Action: New client subscribes
    // Assert: Client receives chunks 0-50 (replay), then 51-100 (live)
}

func TestProxyHandler_StopStream(t *testing.T) {
    // Setup: Mock OpenAI streaming response
    // Action: Client subscribes, receives 50 chunks, then sends stop request
    // Assert: Upstream connection cancelled
    // Assert: Client receives stream_stopped event
    // Assert: Partial response stored in Firestore with stopped=true
}

func TestProxyHandler_StopWithMultipleViewers(t *testing.T) {
    // Setup: Two clients watching same stream
    // Action: Client A stops the stream at chunk 50
    // Assert: Both Client A and Client B receive stop event
    // Assert: Both clients' connections close gracefully
    // Assert: Partial response stored once
}

func TestProxyHandler_StopNonExistentStream(t *testing.T) {
    // Setup: No active stream
    // Action: Send stop request
    // Assert: Returns 404 Not Found
}

func TestProxyHandler_StopCompletedStream(t *testing.T) {
    // Setup: Stream already completed naturally
    // Action: Send stop request
    // Assert: Returns 409 Conflict
}
```

---

### Load Tests

**Scenario 1: Concurrent Streams**
```bash
# vegeta load test
echo "POST http://proxy/chat/completions" | \
  vegeta attack -rate=50/s -duration=60s | \
  vegeta report

# Target: 50 req/s for 60s = 3000 requests
# Expected: p99 latency <500ms, 0 errors
```

**Scenario 2: Multi-Viewer Stress**
```bash
# 100 clients watch same stream
for i in {1..100}; do
  curl -N http://proxy/api/v1/chats/chat_abc/messages/msg_123/stream &
done

# Monitor:
# - Memory usage (should be <50MB for 100 clients)
# - CPU usage (should be <20%)
# - All clients receive complete stream
```

**Scenario 3: Disconnect Storm**
```bash
# 50 clients start watching, all disconnect at 5 seconds
# Upstream should still complete

# Assert:
# - Firestore contains complete message
# - No goroutine leaks
# - Memory cleaned up after TTL
```

---

### Chaos Tests

**Test 1: Firestore Outage**
```go
// Simulate: Firestore writes failing
// Expected: Stream continues, clients receive data
// Expected: Error logged, retry attempted
```

**Test 2: OpenAI Timeout**
```go
// Simulate: OpenAI hangs for 15 minutes
// Expected: Context timeout fires after 10 minutes
// Expected: Clients receive error, session cleaned up
```

**Test 3: Memory Pressure**
```go
// Simulate: 1000 concurrent large streams
// Expected: Old sessions cleaned up aggressively
// Expected: No OOM errors
```

---

## Future Enhancements

### 1. Client-Side Tool Execution (Month 3-4)

**Problem**: Some tools require device access (camera, files, location)

**Solution**: Extend tool registry with client-side execution support
- Primary/secondary device election for multi-viewer coordination
- Tool result submission API: `POST /api/v1/tool-results`
- Client capability detection via headers

**Architecture Ready**: Registry already designed with `ExecutorClient` type

**Examples**:
- `read_local_file` - Access device file system
- `take_screenshot` - Use device camera
- `get_location` - GPS coordinates
- `access_calendar` - Calendar integration

**Benefit**: Full tool ecosystem supporting both remote and local tools

---

### 2. WebSocket Support (Month 4-5)

**Problem**: SSE is one-way, limits bidirectional communication

**Solution**: Add WebSocket endpoint for bidirectional streaming
```
WS /ws/chats/:chatId/stream
→ Client sends: tool execution results, control messages
← Server sends: AI response chunks, tool requests
```

**Benefit**:
- True conversational streaming
- Better client-side tool coordination
- Lower latency for interactive workflows

---

### 3. Cross-Region Replication (Month 6)

**Problem**: Users in EU see high latency to US-based proxy

**Solution**: Deploy StreamManager in multiple regions, use distributed session store (Redis Cluster)

**Benefit**: Global low-latency access

---

## Summary

### What We're Building

1. **StreamManager**: Central coordinator for all active AI response streams
2. **StreamSession**: One session per AI response, reads upstream once, broadcasts to many clients
3. **Resilient Reading**: Upstream reading completes even if all clients disconnect
4. **Multi-Client Broadcast**: Multiple clients can watch same response in real-time
5. **Late-Join Support**: Clients can join mid-stream and get full response via replay (30min TTL)
6. **Stop Control**: Users can cancel generation mid-stream, all viewers notified
7. **Complete Message Storage**: 100% stream completion rate, all tool calls captured
8. **Server-Side Tool Execution**: Tools execute once on server, results broadcast to all clients
9. **Model-to-Provider Routing**: Automatic routing based on model ID (removes X-BASE-URL)
10. **User Message Storage**: Proxy stores user messages to Firestore (encrypted if enabled)

### Key Trade-offs

| Trade-off | Choice | Rationale |
|-----------|--------|-----------|
| Memory vs. Features | Buffer all chunks in memory | Enables replay, reasonable size (~10KB per stream) |
| Latency vs. Reliability | Non-blocking broadcast with timeout | Protects against slow clients, 100ms timeout acceptable |
| Complexity vs. Features | Add session abstraction | Necessary for multi-viewer, manageable complexity |
| Storage vs. Performance | Keep sessions 30min after completion | Enables late-join, auto-cleanup bounds memory |

---

## Next Steps

1. **Review & Approval**: Team reviews this plan, provides feedback
2. **Implementation**: Follow phased rollout (Weeks 1-8)
   - Core streaming (Week 1)
   - Proxy integration + routing + user message storage (Weeks 2-3)
   - Client subscription API (Week 4)
   - Enhanced message storage (Week 5)
   - Server-side tool execution (Weeks 6-7)
   - Observability & hardening (Week 8)
3. **Testing**: Comprehensive testing at each phase
4. **Production Rollout**: Gradual rollout with monitoring (Weeks 9-12)
5. **Iteration**: Gather feedback, plan future enhancements

**Questions?** Please review and provide feedback on:
- Architecture decisions (especially tool execution strategy)
- Model-to-provider routing approach
- User message storage on proxy side
- Removal of X-BASE-URL requirement
- API design changes
- Testing strategy
