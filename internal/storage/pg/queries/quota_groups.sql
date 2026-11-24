-- name: ListQuotaGroupMembers :many
-- The accounts in the same quota group as the given one. Empty when the
-- account is in no group, which makes it a group of one.
SELECT m.user_id
FROM quota_group_members m
WHERE m.group_id = (SELECT g.group_id FROM quota_group_members g WHERE g.user_id = $1)
ORDER BY m.user_id;

-- name: LockQuotaGroups :exec
-- Holds the quota group lock until the transaction ends. Joining two groups
-- moves every member of one; one lock for all groups keeps two joins from
-- splitting a group. Joins happen only when a purchase reaches a new account.
SELECT pg_advisory_xact_lock(hashtextextended('quota_groups', 0));

-- name: GetQuotaGroupID :one
-- An account's group; an account in no group is its own group.
SELECT COALESCE(
    (SELECT group_id FROM quota_group_members WHERE user_id = sqlc.arg(user_id)::text),
    sqlc.arg(user_id)::text
)::text AS group_id;

-- name: SetQuotaGroup :exec
INSERT INTO quota_group_members (user_id, group_id)
VALUES (sqlc.arg(user_id), sqlc.arg(group_id))
ON CONFLICT (user_id) DO UPDATE SET group_id = EXCLUDED.group_id;

-- name: MoveQuotaGroupMembers :exec
-- Moves every member of one group into another.
UPDATE quota_group_members
SET group_id = sqlc.arg(into_group_id)
WHERE group_id = sqlc.arg(from_group_id);
