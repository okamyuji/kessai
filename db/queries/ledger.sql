-- name: InsertLedgerEntry :exec
-- transfer_id + side の複合UNIQUE制約により、Outboxリトライ再実行では二重記帳を防ぐ
INSERT INTO ledger_entries (id, transfer_id, account, side, amount_jpy, payment_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (transfer_id, side) DO NOTHING;

-- name: SumLedgerBalance :one
SELECT
  COALESCE(SUM(CASE WHEN side = 'debit' THEN amount_jpy ELSE 0 END), 0)::BIGINT AS debit_total,
  COALESCE(SUM(CASE WHEN side = 'credit' THEN amount_jpy ELSE 0 END), 0)::BIGINT AS credit_total
FROM ledger_entries
WHERE account = $1;

-- name: ListLedgerByPayment :many
SELECT id, transfer_id, account, side, amount_jpy, payment_id, created_at
FROM ledger_entries
WHERE payment_id = $1
ORDER BY created_at ASC;
