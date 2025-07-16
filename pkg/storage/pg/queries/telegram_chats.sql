-- name: CreateTelegramChat :one
INSERT INTO telegram_chats (chat_id, chat_uuid)
VALUES ($1, $2)
ON CONFLICT (chat_id) DO UPDATE SET
    chat_uuid = EXCLUDED.chat_uuid,
    updated_at = NOW()
RETURNING *;

-- name: GetTelegramChatByChatID :one
SELECT * FROM telegram_chats
WHERE chat_id = $1;

-- name: GetTelegramChatByChatUUID :one
SELECT * FROM telegram_chats
WHERE chat_uuid = $1;

-- name: ListTelegramChats :many
SELECT * FROM telegram_chats
ORDER BY created_at DESC;

-- name: DeleteTelegramChat :exec
DELETE FROM telegram_chats
WHERE chat_id = $1; 