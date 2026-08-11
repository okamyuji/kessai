-- name: GetProduct :one
SELECT id, name, price_jpy, tokusho_snapshot, created_at
FROM products
WHERE id = $1;

-- name: ListProducts :many
SELECT id, name, price_jpy, tokusho_snapshot, created_at
FROM products
ORDER BY created_at DESC
LIMIT $1;

-- name: UpsertProduct :exec
INSERT INTO products (id, name, price_jpy, tokusho_snapshot)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, price_jpy = EXCLUDED.price_jpy, tokusho_snapshot = EXCLUDED.tokusho_snapshot;
