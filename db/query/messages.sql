-- name: CreateMessage :one
INSERT INTO messages (message_id, conversation_id, sequence_id, generation, type, llm_data, user_data, usage_data, display_data, excluded_from_context, llm_api_url, model_name)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetNextSequenceID :one
SELECT COALESCE(MAX(sequence_id), 0) + 1 
FROM messages 
WHERE conversation_id = ?;

-- name: GetMaxSequenceIDsForAllConversations :many
SELECT conversation_id, CAST(COALESCE(MAX(sequence_id), 0) AS INTEGER) AS max_sequence_id
FROM messages
GROUP BY conversation_id;

-- name: GetMessage :one
SELECT * FROM messages
WHERE message_id = ?;

-- name: ListMessages :many
SELECT * FROM messages
WHERE conversation_id = ?
ORDER BY sequence_id ASC;

-- name: ListMessagesForContext :many
SELECT m.* FROM messages m
INNER JOIN conversations c ON m.conversation_id = c.conversation_id
WHERE m.conversation_id = ?
  AND m.excluded_from_context = FALSE
  AND m.generation = c.current_generation
ORDER BY m.sequence_id ASC;

-- name: ListMessagesPaginated :many
SELECT * FROM messages
WHERE conversation_id = ?
ORDER BY sequence_id ASC
LIMIT ? OFFSET ?;

-- name: CopyMessagesForFork :exec
-- Copies the messages of a source conversation's current generation, up to and
-- including a cutoff sequence_id, into a destination conversation. The copies
-- are renumbered to generation 1 (the destination starts a fresh generation
-- history), get new message_ids, and preserve content, ordering, and original
-- timestamps. Used to fork a conversation.
INSERT INTO messages (message_id, conversation_id, sequence_id, generation, type, llm_data, user_data, usage_data, display_data, excluded_from_context, llm_api_url, model_name, forked_from_message_id, created_at)
SELECT lower(hex(randomblob(16))), sqlc.arg('dest_conversation_id'), m.sequence_id, 1, m.type, m.llm_data, m.user_data, m.usage_data, m.display_data, m.excluded_from_context, m.llm_api_url, m.model_name, m.message_id, m.created_at
FROM messages m
WHERE m.conversation_id = sqlc.arg('source_conversation_id')
  AND m.sequence_id <= sqlc.arg('cutoff_sequence_id')
  AND m.generation = sqlc.arg('source_generation')
ORDER BY m.sequence_id ASC;

-- name: ListMessagesByType :many
SELECT * FROM messages
WHERE conversation_id = ? AND type = ?
ORDER BY sequence_id ASC;

-- name: GetLatestMessage :one
SELECT * FROM messages
WHERE conversation_id = ?
ORDER BY sequence_id DESC
LIMIT 1;

-- name: DeleteMessage :exec
DELETE FROM messages
WHERE message_id = ?;

-- name: DeleteConversationMessages :exec
DELETE FROM messages
WHERE conversation_id = ?;

-- name: CountMessagesInConversation :one
SELECT COUNT(*) FROM messages
WHERE conversation_id = ?;

-- name: CountMessagesByType :one
SELECT COUNT(*) FROM messages
WHERE conversation_id = ? AND type = ?;

-- name: CountConsecutiveMessagesByType :one
SELECT COUNT(*) FROM messages m
WHERE m.conversation_id = sqlc.arg('conversation_id')
  AND m.generation = sqlc.arg('generation')
  AND m.type = sqlc.arg('type')
  AND m.sequence_id > COALESCE(
    (SELECT MAX(prev.sequence_id) FROM messages prev
     WHERE prev.conversation_id = sqlc.arg('conversation_id')
       AND prev.generation = sqlc.arg('generation')
       AND prev.type != sqlc.arg('type')),
    0);

-- name: ListMessagesTail :many
-- Returns the last N messages in ascending order. If fewer than N
-- exist, returns all of them.
SELECT * FROM (
  SELECT * FROM messages
  WHERE conversation_id = ?
  ORDER BY sequence_id DESC
  LIMIT ?
) ORDER BY sequence_id ASC;

-- name: ListMessagesSince :many
SELECT * FROM messages
WHERE conversation_id = ? AND sequence_id > ?
ORDER BY sequence_id ASC;

-- name: UpdateMessageUserData :exec
UPDATE messages SET user_data = ? WHERE message_id = ?;

-- name: UpdateMessageExcludedFromContext :exec
UPDATE messages SET excluded_from_context = ? WHERE message_id = ?;

-- name: ListAgentMessagesSinceLastUser :many
-- Returns the agent messages produced during the most recent user turn,
-- ordered newest-first. "Most recent user turn" = all agent messages
-- whose sequence_id is greater than the sequence_id of the most recent
-- user message (or all agent messages if there is no user message yet,
-- e.g. orchestrator-spawned conversations). Used by the end-of-turn
-- notification builder to pick a useful body line.
SELECT m.message_id, m.conversation_id, m.sequence_id, m.type,
       m.llm_data, m.user_data, m.usage_data, m.created_at,
       m.display_data, m.excluded_from_context, m.generation,
       m.llm_api_url, m.model_name, m.forked_from_message_id
FROM messages m
WHERE m.conversation_id = ? AND m.type = 'agent'
  AND m.sequence_id > COALESCE(
    (SELECT MAX(u.sequence_id) FROM messages u
     WHERE u.conversation_id = ? AND u.type = 'user'),
    0)
ORDER BY m.sequence_id DESC;
