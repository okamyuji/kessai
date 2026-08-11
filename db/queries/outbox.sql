-- name: EnqueueOutboxEvent :one
INSERT INTO outbox_events (id, event_type, payload, next_attempt_at)
VALUES ($1, $2, $3, now())
RETURNING id, event_type, payload, status, retry_count, next_attempt_at, created_at, processed_at;

-- name: FetchPendingOutbox :many
-- 複数ワーカーの二重取得を防ぐためSKIP LOCKEDを使う
SELECT id, event_type, payload, status, retry_count, next_attempt_at, created_at, processed_at
FROM outbox_events
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at ASC
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxProcessing :exec
UPDATE outbox_events SET status = 'processing' WHERE id = $1;

-- name: MarkOutboxDone :exec
UPDATE outbox_events SET status = 'done', processed_at = now(), last_error = NULL WHERE id = $1;

-- name: RescheduleOutbox :exec
UPDATE outbox_events
SET status = 'pending', retry_count = retry_count + 1, next_attempt_at = $2, last_error = $3
WHERE id = $1;

-- name: MarkOutboxFailed :exec
UPDATE outbox_events SET status = 'failed', last_error = $2, retry_count = retry_count + 1 WHERE id = $1;

-- name: ListOutboxEvents :many
SELECT id, event_type, payload, status, retry_count, next_attempt_at, last_error, created_at, processed_at
FROM outbox_events
ORDER BY created_at DESC
LIMIT $1;
