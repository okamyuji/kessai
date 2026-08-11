-- name: TryInsertIdempotency :one
-- 冪等性キー行を挿入する。既存キーとの競合時は0行を返す（呼び出し側でGetIdempotencyを実行して結果を返却する）
INSERT INTO idempotency_keys (key, request_hash, payment_id, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key) DO NOTHING
RETURNING key, request_hash, response_snapshot, payment_id, expires_at, created_at;

-- name: GetIdempotency :one
SELECT key, request_hash, response_snapshot, payment_id, expires_at, created_at
FROM idempotency_keys
WHERE key = $1;

-- name: GetIdempotencyByPaymentID :one
SELECT key, request_hash, response_snapshot, payment_id, expires_at, created_at
FROM idempotency_keys
WHERE payment_id = $1;

-- name: SetIdempotencyResponse :exec
UPDATE idempotency_keys
SET response_snapshot = $2, payment_id = COALESCE($3, payment_id)
WHERE key = $1;

-- name: DeleteExpiredIdempotency :execrows
DELETE FROM idempotency_keys WHERE expires_at < now();
