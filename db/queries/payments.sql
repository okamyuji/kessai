-- name: CreatePayment :one
INSERT INTO payments (id, product_id, amount_jpy, state)
VALUES ($1, $2, $3, 'created')
RETURNING id, product_id, amount_jpy, refunded_jpy, state, stripe_payment_intent_id, created_at, updated_at;

-- name: GetPayment :one
SELECT id, product_id, amount_jpy, refunded_jpy, state, stripe_payment_intent_id, created_at, updated_at
FROM payments
WHERE id = $1;

-- name: GetPaymentForUpdate :one
SELECT id, product_id, amount_jpy, refunded_jpy, state, stripe_payment_intent_id, created_at, updated_at
FROM payments
WHERE id = $1
FOR UPDATE;

-- name: ListPayments :many
SELECT id, product_id, amount_jpy, refunded_jpy, state, stripe_payment_intent_id, created_at, updated_at
FROM payments
WHERE ($1::payment_state IS NULL OR state = $1::payment_state)
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: UpdatePaymentState :one
UPDATE payments
SET state = $2, updated_at = now()
WHERE id = $1 AND state = $3
RETURNING id, product_id, amount_jpy, refunded_jpy, state, stripe_payment_intent_id, created_at, updated_at;

-- name: SetStripePaymentIntent :exec
UPDATE payments SET stripe_payment_intent_id = $2, updated_at = now() WHERE id = $1;

-- name: AddRefundedAmount :one
UPDATE payments
SET refunded_jpy = refunded_jpy + $2, updated_at = now()
WHERE id = $1
RETURNING id, amount_jpy, refunded_jpy, state;

-- name: InsertPaymentTransition :exec
INSERT INTO payment_transitions (id, payment_id, from_state, to_state, event, actor)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListPaymentTransitions :many
SELECT id, payment_id, from_state, to_state, event, actor, created_at
FROM payment_transitions
WHERE payment_id = $1
ORDER BY created_at ASC;
