# Deep Research Functionality

The Deep Research (DeepR) functionality provides a WebSocket-based proxy service that enables users to perform advanced research queries through a dedicated backend service. This feature implements a freemium model with usage tracking and subscription-based access control.

## Architecture Overview

The Deep Research functionality consists of three main components:

1. **WebSocket Handler** (`internal/deepr/handlers.go`) - Manages incoming WebSocket connections
2. **Service Layer** (`internal/deepr/service.go`) - Handles business logic and backend communication
3. **Data Models** (`internal/deepr/models.go`) - Defines message structures
4. **Firebase Integration** (`internal/auth/firebase_client.go`) - Manages usage tracking

## API Endpoint

```
GET /api/deepresearch/ws?chat_id=<chat_id>
```

**Authentication**: Required (JWT token)
**Protocol**: WebSocket upgrade from HTTP

## Request Flow

### 1. Connection Establishment

1. Client initiates WebSocket connection to `/api/deepresearch/ws`
2. Server validates JWT authentication
3. Server extracts `chat_id` from query parameters
4. HTTP connection is upgraded to WebSocket
5. Service instance is created with dependencies

### 2. Subscription Validation

The system implements a two-tier access model:

#### Pro Users
- Users with active Pro subscriptions get **2 sessions per calendar month**
- Usage is tracked for quota enforcement and analytics
- Quota resets on the 1st of each month

#### Freemium Users
- Limited to **one free deep research session** per user
- Usage is tracked in Firebase Firestore
- Subsequent attempts are blocked with upgrade prompt

### 3. Backend Connection

1. Server constructs WebSocket URL to deep research backend:
   ```
   ws://{DEEP_RESEARCH_WS}/deep_research/{user_id}/{chat_id}/
   ```
2. Establishes connection to external deep research service
3. Creates bidirectional message forwarding

### 4. Message Relay

The service acts as a transparent proxy:

- **Client → Backend**: Forwards all client messages to deep research backend
- **Backend → Client**: Forwards all backend responses to client
- **Error Handling**: Graceful handling of connection drops and errors

## Data Models

### Message Structure
```go
type Message struct {
    Type    string `json:"type"`
    Content string `json:"content"`
    Data    string `json:"data,omitempty"`
}
```

### Request Structure
```go
type Request struct {
    Query string `json:"query"`
    Type  string `json:"type"`
}
```

### Response Structure
```go
type Response struct {
    Type    string `json:"type"`
    Content string `json:"content"`
    Status  string `json:"status,omitempty"`
}
```

## Usage Tracking

### Firebase Firestore Schema

Collection: `deep_research_usage`
Document ID: `{user_id}`

```go
type DeepResearchUsage struct {
    UserID            string               `firestore:"user_id"`
    FirstUsedAt       time.Time            `firestore:"first_used_at"`
    LastUsedAt        time.Time            `firestore:"last_used_at"`
    UsageCount        int64                `firestore:"usage_count"`
    CompletedSessions map[string]time.Time `firestore:"completed_sessions"` // Map of chat_id to completion timestamp
}
```

### Tracking Logic

#### Freemium Users
- **Quota**: 1 completed session lifetime
- `CompletedSessions`: Tracks all completed sessions with their completion timestamps
- Quota is checked by counting total completed sessions
- Error code: `deep_research_quota_exceeded`

#### Pro Users
- **Quota**: 2 completed sessions per calendar month
- `CompletedSessions`: Tracks all completed sessions with their completion timestamps
- Quota is checked by counting sessions completed in the current calendar month
- Quota resets on the 1st of each month
- `UsageCount`: Also incremented for analytics

## Error Handling

### Authentication Errors
- **401 Unauthorized**: Missing or invalid JWT token
- **400 Bad Request**: Missing `chat_id` parameter

### Subscription Errors
- **deep_research_quota_exceeded**: User exceeded their quota (freemium: 1 lifetime, pro: 2/month)
- **ACTIVE_SESSION_EXISTS**: Freemium user attempting to start a new session while another is active
- **Subscription verification failure**: Database errors

### Connection Errors
- **Backend unavailable**: `DEEP_RESEARCH_WS` not configured
- **Connection failure**: Deep research service unreachable
- **WebSocket errors**: Connection drops and protocol errors

## Configuration

### Environment Variables

- `DEEP_RESEARCH_WS`: Hostname of the deep research backend service
- Firebase configuration for usage tracking

### Dependencies

- **Logger**: Structured logging with context
- **Request Tracking Service**: Subscription validation
- **Firebase Client**: Usage tracking and analytics
- **WebSocket Library**: Gorilla WebSocket for connection management

## Security Considerations

1. **Authentication**: All connections require valid JWT tokens
2. **Origin Validation**: WebSocket upgrader allows all origins (configurable)
3. **User Isolation**: Each connection is scoped to authenticated user
4. **Chat Isolation**: Messages are scoped to specific chat sessions

## Monitoring and Logging

The service provides comprehensive logging:

- Connection establishment and termination
- Authentication status
- Subscription validation results
- Message forwarding (client ↔ backend)
- Error conditions and stack traces
- Usage tracking events

All logs include structured fields for user ID, chat ID, and request context.

## Usage Examples

### Successful Connection (Pro User)
```
1. Client connects with valid JWT
2. Server validates Pro subscription
3. Server checks quota (completed sessions this month < 2)
4. Backend connection established
5. Bidirectional message relay begins
```

### Freemium User (First Use)
```
1. Client connects with valid JWT
2. Server checks quota (completed sessions count = 0)
3. Backend connection established
4. Session proceeds normally
5. On completion, session added to completed_sessions map
```

### Freemium User (Quota Exceeded)
```
1. Client connects with valid JWT
2. Server checks quota (completed sessions count >= 1)
3. Connection rejected with deep_research_quota_exceeded error
4. Upgrade prompt sent to client
```

### Pro User (Monthly Quota Exceeded)
```
1. Client connects with valid JWT
2. Server checks quota (completed sessions this month >= 2)
3. Connection rejected with deep_research_quota_exceeded error
4. Error message includes reset date (1st of next month)
```

This architecture provides a scalable, secure, and monetizable deep research service with proper usage controls and comprehensive monitoring.

## Reconnection and Session Persistence

The deep research service now supports automatic session persistence and reconnection. This allows iOS clients to disconnect (e.g., app backgrounded or killed) and reconnect without losing progress.

**Key Features:**
- **Session Persistence**: Messages are stored to disk as they arrive from the backend
- **Message State Tracking**: Each message is marked as sent/unsent (delivered to iOS or not)
- **Backend Persistence**: Connection to deep research backend stays active even when iOS disconnects
- **Automatic Recovery**: On reconnection, all unsent messages are delivered automatically
- **Session Completion Detection**: Recognizes when research is complete (final report or error)

For detailed information about the reconnection feature, see [Deep Research Reconnection Documentation](./deep-research-reconnection.md).
