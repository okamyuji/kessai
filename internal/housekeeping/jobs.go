// Package housekeeping 定期実行の失効・掃除ジョブです（03-basic-design 4.4節）。
// 3つのジョブを1分間隔で走らせる想定。
package housekeeping

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// ExpiryPolicy 各種失効時間の設定
type ExpiryPolicy struct {
	CheckoutExpiryMinutes int // created のまま検出する上限
	AuthExpiryDays        int // authorized のまま検出する上限
}

// PGQuerier 内部で使う最小のExec/Query。PoolAdapterが満たす。
type PGQuerier interface {
	QueryScalarStrings(ctx context.Context, sql string, args ...any) ([]string, error)
	Exec(ctx context.Context, sql string, args ...any) error
	// InTx fn内の全操作を1つのDBトランザクションで実行する
	InTx(ctx context.Context, fn func(q PGQuerier) error) error
}

// Runner 依存を注入した実行器
type Runner struct {
	Queries *sqlc.Queries
	PG      PGQuerier
	IDs     idgen.Generator
	Logger  *slog.Logger
	Policy  ExpiryPolicy
	Now     func() time.Time
}

// New Runnerを構築します
func New(q *sqlc.Queries, pg PGQuerier, ids idgen.Generator, logger *slog.Logger, policy ExpiryPolicy) *Runner {
	return &Runner{
		Queries: q, PG: pg, IDs: ids, Logger: logger, Policy: policy, Now: time.Now,
	}
}

// RunOnce 3つのジョブを順に実行し、それぞれの件数を返します
func (r *Runner) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	n, err := r.expireCreated(ctx)
	if err != nil {
		return res, fmt.Errorf("expireCreated: %w", err)
	}
	res.ExpiredCreated = n
	n, err = r.expireAuthorized(ctx)
	if err != nil {
		return res, fmt.Errorf("expireAuthorized: %w", err)
	}
	res.ExpiredAuthorized = n
	n64, err := r.Queries.DeleteExpiredIdempotency(ctx)
	if err != nil {
		return res, fmt.Errorf("DeleteExpiredIdempotency: %w", err)
	}
	res.DeletedIdempotency = int(n64)
	return res, nil
}

// Result RunOnceの返り値
type Result struct {
	ExpiredCreated     int
	ExpiredAuthorized  int
	DeletedIdempotency int
}

// expireCreated created のまま checkout_expiry_minutes 経過した payments を expired に遷移し履歴を追記
func (r *Runner) expireCreated(ctx context.Context) (int, error) {
	cutoff := r.Now().Add(-time.Duration(r.Policy.CheckoutExpiryMinutes) * time.Minute)
	return r.expireBy(ctx, "created", cutoff)
}

// expireAuthorized authorized のまま auth_expiry_days 経過した payments を expired に遷移
func (r *Runner) expireAuthorized(ctx context.Context) (int, error) {
	cutoff := r.Now().Add(-time.Duration(r.Policy.AuthExpiryDays) * 24 * time.Hour)
	return r.expireBy(ctx, "authorized", cutoff)
}

// expireBy 指定状態で cutoff より古いpaymentsをexpiredへ遷移し、遷移履歴も1件挿入します。
// 手順: (1) 対象IDをRETURNINGで取得して状態を expired にUPDATE、(2) 各IDに対して
// アプリ側でULIDを生成し payment_transitions を1件ずつINSERTする。
// (1)(2)は同一トランザクションで確定する。片方だけ残ると「expiredなのに履歴がない」決済が生まれるため。
func (r *Runner) expireBy(ctx context.Context, fromState string, cutoff time.Time) (int, error) {
	const updateSQL = `
UPDATE payments
   SET state = 'expired', updated_at = now()
 WHERE state = $1::payment_state AND updated_at < $2
 RETURNING id`
	const insertSQL = `
INSERT INTO payment_transitions (id, payment_id, from_state, to_state, event, actor)
VALUES ($1, $2, $3::payment_state, 'expired'::payment_state, 'Expire', 'housekeeping')`
	var count int
	err := r.PG.InTx(ctx, func(q PGQuerier) error {
		ids, err := q.QueryScalarStrings(ctx, updateSQL, fromState, cutoff)
		if err != nil {
			return err
		}
		for _, pid := range ids {
			if err := q.Exec(ctx, insertSQL, r.IDs.New(), pid, fromState); err != nil {
				return fmt.Errorf("insert transition: %w", err)
			}
		}
		count = len(ids)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
