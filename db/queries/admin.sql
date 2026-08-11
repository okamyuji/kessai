-- name: CreateAdminUser :one
INSERT INTO admin_users (id, email, password_hash)
VALUES ($1, $2, $3)
ON CONFLICT (email) DO NOTHING
RETURNING id, email, password_hash, created_at;

-- name: GetAdminUserByEmail :one
SELECT id, email, password_hash, created_at
FROM admin_users
WHERE email = $1;

-- name: CreateAdminSession :exec
INSERT INTO admin_sessions (id, admin_user_id, expires_at)
VALUES ($1, $2, $3);

-- name: GetAdminSession :one
SELECT id, admin_user_id, expires_at, created_at
FROM admin_sessions
WHERE id = $1 AND expires_at > now();

-- name: DeleteAdminSession :exec
DELETE FROM admin_sessions WHERE id = $1;

-- name: InsertAuditLog :exec
INSERT INTO audit_logs (id, actor, action, subject_type, subject_id, detail)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListAuditLogs :many
SELECT id, actor, action, subject_type, subject_id, detail, created_at
FROM audit_logs
ORDER BY created_at DESC
LIMIT $1;
