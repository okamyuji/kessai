package housekeeping

import (
	"context"

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
