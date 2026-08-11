-- name: TryInsertWebhookEvent :one
-- stripe_event_idのUNIQUE制約で再配信を冪等化する。競合時は0行を返す
INSERT INTO webhook_events (id, stripe_event_id, event_type, payload)
VALUES ($1, $2, $3, $4)
ON CONFLICT (stripe_event_id) DO NOTHING
RETURNING id, stripe_event_id, event_type, payload, status, received_at, processed_at;

-- name: MarkWebhookProcessed :exec
UPDATE webhook_events SET status = 'processed', processed_at = now() WHERE id = $1;

-- name: ListWebhookEvents :many
SELECT id, stripe_event_id, event_type, payload, status, received_at, processed_at
FROM webhook_events
ORDER BY received_at DESC
LIMIT $1;
