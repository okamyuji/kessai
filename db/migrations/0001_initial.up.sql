-- kessai 初期スキーマ。設計は docs/03-basic-design.md の 3 章に対応します。
-- 金額はすべて円単位のBIGINT整数です（ADR-0006）。主キーはULID文字列（ADR-0004）。

CREATE TABLE products (
    id                CHAR(26) PRIMARY KEY,
    name              TEXT NOT NULL,
    price_jpy         BIGINT NOT NULL CHECK (price_jpy > 0),
    tokusho_snapshot  JSONB NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE payment_state AS ENUM (
    'created', 'authorized', 'captured',
    'canceled', 'expired', 'failed',
    'partially_refunded', 'refunded'
);

CREATE TABLE payments (
    id                        CHAR(26) PRIMARY KEY,
    product_id                CHAR(26) NOT NULL REFERENCES products(id),
    amount_jpy                BIGINT NOT NULL CHECK (amount_jpy > 0),
    refunded_jpy              BIGINT NOT NULL DEFAULT 0 CHECK (refunded_jpy >= 0 AND refunded_jpy <= amount_jpy),
    state                     payment_state NOT NULL DEFAULT 'created',
    stripe_payment_intent_id  TEXT UNIQUE,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payments_state_created_at_idx ON payments (state, created_at DESC);
CREATE INDEX payments_updated_at_idx ON payments (updated_at DESC);

CREATE TABLE payment_transitions (
    id          CHAR(26) PRIMARY KEY,
    payment_id  CHAR(26) NOT NULL REFERENCES payments(id),
    from_state  payment_state NOT NULL,
    to_state    payment_state NOT NULL,
    event       TEXT NOT NULL,
    actor       TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX payment_transitions_payment_id_created_at_idx ON payment_transitions (payment_id, created_at);

CREATE TABLE idempotency_keys (
    key                CHAR(26) PRIMARY KEY,
    request_hash       BYTEA NOT NULL,
    response_snapshot  JSONB,
    payment_id         CHAR(26) REFERENCES payments(id),
    expires_at         TIMESTAMPTZ NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

CREATE TYPE ledger_side AS ENUM ('debit', 'credit');
CREATE TYPE ledger_account AS ENUM ('psp_receivable', 'sales', 'refunds');

CREATE TABLE ledger_entries (
    id          CHAR(26) PRIMARY KEY,
    transfer_id TEXT NOT NULL,
    account     ledger_account NOT NULL,
    side        ledger_side NOT NULL,
    amount_jpy  BIGINT NOT NULL CHECK (amount_jpy > 0),
    payment_id  CHAR(26) NOT NULL REFERENCES payments(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transfer_id, side)
);
CREATE INDEX ledger_entries_payment_id_idx ON ledger_entries (payment_id);

CREATE TYPE outbox_status AS ENUM ('pending', 'processing', 'done', 'failed');

CREATE TABLE outbox_events (
    id               CHAR(26) PRIMARY KEY,
    event_type       TEXT NOT NULL,
    payload          JSONB NOT NULL,
    status           outbox_status NOT NULL DEFAULT 'pending',
    retry_count      INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at     TIMESTAMPTZ
);
CREATE INDEX outbox_events_pending_idx ON outbox_events (status, next_attempt_at) WHERE status = 'pending';

CREATE TYPE webhook_status AS ENUM ('received', 'processed', 'failed');

CREATE TABLE webhook_events (
    id                 CHAR(26) PRIMARY KEY,
    stripe_event_id    TEXT NOT NULL UNIQUE,
    event_type         TEXT NOT NULL,
    payload            JSONB NOT NULL,
    status             webhook_status NOT NULL DEFAULT 'received',
    received_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at       TIMESTAMPTZ
);
CREATE INDEX webhook_events_received_at_idx ON webhook_events (received_at DESC);

CREATE TABLE admin_users (
    id             CHAR(26) PRIMARY KEY,
    email          TEXT NOT NULL UNIQUE,
    password_hash  TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE admin_sessions (
    id             CHAR(26) PRIMARY KEY,
    admin_user_id  CHAR(26) NOT NULL REFERENCES admin_users(id),
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX admin_sessions_expires_at_idx ON admin_sessions (expires_at);

CREATE TABLE audit_logs (
    id            CHAR(26) PRIMARY KEY,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    subject_type  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    detail        JSONB NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
