package housekeeping

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolAdapter pgxpool.Pool を PGQuerier として使えるようにする薄いアダプタ
type PoolAdapter struct {
	Pool *pgxpool.Pool
}

// Exec 単発Execを実行し、CommandTagを破棄してエラーだけ返します
func (a *PoolAdapter) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := a.Pool.Exec(ctx, sql, args...)
	return err
}

// QueryScalarStrings 単カラムのTEXT/CHARを行ごとに文字列で取得します（RETURNING id用）
func (a *PoolAdapter) QueryScalarStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := a.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

// InTx fn内の全操作を1つのDBトランザクションで実行します
func (a *PoolAdapter) InTx(ctx context.Context, fn func(q PGQuerier) error) error {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(&txAdapter{tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// txAdapter pgx.Tx を PGQuerier として使うアダプタ。すでにトランザクション内のためInTxは入れ子にしない
type txAdapter struct {
	tx pgx.Tx
}

func (a *txAdapter) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := a.tx.Exec(ctx, sql, args...)
	return err
}

func (a *txAdapter) QueryScalarStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := a.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

func (a *txAdapter) InTx(ctx context.Context, fn func(q PGQuerier) error) error {
	return fn(a)
}

func scanStrings(rows pgx.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
