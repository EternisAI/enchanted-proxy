-- name: AddDeepResearchMessage :exec
INSERT INTO deep_research_messages (id, user_id, chat_id, session_id, message, message_type, sent, created_at, sent_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetUnsentMessages :many
SELECT id, user_id, chat_id, session_id, message, message_type, sent, created_at, sent_at
FROM deep_research_messages
WHERE session_id = $1 AND sent = FALSE
ORDER BY created_at ASC;

-- name: MarkMessageAsSent :exec
UPDATE deep_research_messages
SET sent = TRUE, sent_at = NOW()
WHERE id = $1;

-- name: MarkAllMessagesAsSent :exec
UPDATE deep_research_messages
SET sent = TRUE, sent_at = NOW()
WHERE session_id = $1 AND sent = FALSE;

-- name: GetSessionMessages :many
SELECT id, user_id, chat_id, session_id, message, message_type, sent, created_at, sent_at
FROM deep_research_messages
WHERE session_id = $1
ORDER BY created_at ASC;

-- name: DeleteSessionMessages :exec
DELETE FROM deep_research_messages
WHERE session_id = $1;

-- name: GetSessionMessageCount :one
SELECT COUNT(*) as total_messages
FROM deep_research_messages
WHERE session_id = $1;

-- name: GetUnsentMessageCount :one
SELECT COUNT(*) as unsent_count
FROM deep_research_messages
WHERE session_id = $1 AND sent = FALSE;
