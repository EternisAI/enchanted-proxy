# Deep Research Reconnection Feature

## Overview

The Deep Research service now supports automatic session persistence and reconnection, allowing iOS clients to disconnect and reconnect without losing progress or messages.

## How It Works

### Architecture

1. **Session Persistence**: All messages from the deep research backend are stored in JSON files on the enchanted-proxy server
2. **Message State Tracking**: Each message is marked as "sent" or "unsent" (sent to iOS client)
3. **Backend Connection Persistence**: The connection between enchanted-proxy and deep research backend remains active even when iOS disconnects
4. **Automatic Reconnection**: When iOS reconnects, it receives all unsent messages and continues listening for new ones

### Message Flow

#### Initial Connection
```
iOS App → WebSocket → Enchanted Proxy → WebSocket → Deep Research Backend
                            ↓
                      JSON Storage
                   (Messages persisted)
```

#### iOS Disconnects
```
Deep Research Backend → Enchanted Proxy → JSON Storage (messages marked as "unsent")
    (continues running)      ↓
                      Backend stays connected
```

#### iOS Reconnects
```
iOS App → WebSocket → Enchanted Proxy
                            ↓
                      1. Loads session state
                      2. Sends unsent messages
                      3. Continues receiving new messages
```

## Key Components

### 1. Storage Layer (`storage.go`)

Handles persistence of messages and session state:

- **SessionState**: Tracks session metadata (backend connection status, completion status, etc.)
- **PersistedMessage**: Individual message with sent/unsent status
- **Storage Methods**:
  - `LoadSession()`: Load session state from disk
  - `SaveSession()`: Save session state to disk
  - `AddMessage()`: Store new message with sent status
  - `GetUnsentMessages()`: Retrieve messages not yet sent to client
  - `MarkMessageAsSent()`: Mark message as delivered
  - `IsSessionComplete()`: Check if research is complete

### 2. Session Manager (`session_manager.go`)

Manages active backend connections:

- **ActiveSession**: Represents a live backend connection with multiple client connections
- **SessionManager Methods**:
  - `CreateSession()`: Create new backend session
  - `GetSession()`: Retrieve active session
  - `AddClientConnection()`: Add iOS client to session
  - `RemoveClientConnection()`: Remove iOS client from session
  - `BroadcastToClients()`: Send message to all connected clients
  - `HasActiveBackend()`: Check if backend connection exists

### 3. Service Updates (`service.go`)

Enhanced service logic:

- **Reconnection Detection**: Checks if backend session exists for userID/chatID
- **Message Persistence**: Stores every message with sent/unsent status
- **Unsent Message Delivery**: Sends accumulated messages on reconnection
- **Session Completion Detection**: Recognizes final reports and errors

## Session States

### Message States

1. **sent: true**: Message successfully delivered to iOS client
2. **sent: false**: Message received from backend but not yet delivered to iOS

### Session Completion Conditions

A session is considered complete when:
1. **Final Report Received**: Message contains `final_report` field with content
2. **Error Occurred**: Message has `type: "error"` or contains `error` field

## Storage Format

### Session File Location

Default: `./deepr_sessions/session_{userID}_{chatID}.json`

Can be configured via environment variable: `DEEPR_STORAGE_PATH`

### Session File Structure

```json
{
  "user_id": "abc123",
  "chat_id": "chat456",
  "messages": [
    {
      "id": "msg-uuid-1",
      "user_id": "abc123",
      "chat_id": "chat456",
      "message": "{\"type\":\"status\",\"content\":\"Starting research...\"}",
      "sent": true,
      "timestamp": "2025-10-08T10:30:00Z",
      "message_type": "status"
    },
    {
      "id": "msg-uuid-2",
      "user_id": "abc123",
      "chat_id": "chat456",
      "message": "{\"type\":\"update\",\"content\":\"Analyzing sources...\"}",
      "sent": false,
      "timestamp": "2025-10-08T10:31:00Z",
      "message_type": "update"
    }
  ],
  "backend_connected": true,
  "last_activity": "2025-10-08T10:31:00Z",
  "final_report_received": false,
  "error_occurred": false
}
```

## Reconnection Behavior

### On Reconnection

1. **Check Session**: Determine if backend session exists
2. **Send Unsent Messages**: Deliver all messages marked as unsent
3. **Check Completion**:
   - If `final_report_received` or `error_occurred`: Send final message and close
   - Otherwise: Continue listening for new messages
4. **Join Active Session**: Add client to existing session for real-time updates

### Multiple Clients

The system supports multiple iOS clients connected to the same session:

- Messages are broadcast to all connected clients
- Each client can disconnect/reconnect independently
- Backend connection persists as long as research is ongoing

## Configuration

### Environment Variables

```bash
# Storage path for session files (optional)
DEEPR_STORAGE_PATH=/path/to/sessions

# Deep research backend WebSocket URL (required)
DEEP_RESEARCH_WS=your-backend-host:port
```

### Default Values

- Storage Path: `./deepr_sessions`
- Session file naming: `session_{userID}_{chatID}.json`

## Cleanup

Session files should be cleaned up periodically using:

```go
storage.CleanupOldSessions(maxAge time.Duration)
```

Recommended: Clean up sessions older than 24-48 hours

## Error Handling

### Storage Failures

If storage operations fail:
- Error is logged
- Message delivery continues
- Reconnection may not have full history

### Backend Disconnection

If backend connection drops:
- Session marked as disconnected
- Existing messages remain available
- New connection attempts will create fresh session

### Client Disconnection

If iOS client disconnects:
- Backend connection remains active
- Messages continue to be stored as unsent
- Client can reconnect anytime

## Message Types

The system recognizes these message types:

1. **status**: Progress updates
2. **update**: Research updates
3. **error**: Error messages (marks session as complete)
4. **final**: Messages with `final_report` field (marks session as complete)

## Usage Example

### iOS Client Flow

```
1. Connect to /api/deepresearch/ws?chat_id=123
2. Send research query
3. Receive status updates
4. (App backgrounded/disconnected)
5. (Messages continue arriving at backend)
6. Reconnect to /api/deepresearch/ws?chat_id=123
7. Receive all unsent messages
8. Continue receiving new messages
9. Receive final report
10. Session complete
```

## Monitoring

Log messages indicate:
- Session creation/removal
- Client connections/disconnections
- Message persistence status
- Reconnection events
- Unsent message delivery

Example log:
```
INFO: created new session user_id=abc123 chat_id=chat456
INFO: message stored sent=false type=status
INFO: detected reconnection user_id=abc123 chat_id=chat456
INFO: sending unsent messages count=5
```

## Testing Recommendations

1. **Test Disconnection**: Kill iOS app during research
2. **Test Reconnection**: Reopen app and verify messages received
3. **Test Multiple Clients**: Connect from multiple devices
4. **Test Error Handling**: Verify error messages mark session complete
5. **Test Final Report**: Verify final report marks session complete
6. **Test Storage**: Check session files are created correctly
