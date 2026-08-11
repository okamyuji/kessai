// Package outbox Transactional Outbox パターンのリレーワーカー実装です（ADR-0009）。
// SELECT FOR UPDATE SKIP LOCKED でイベントを取り出し、Handler に渡します。
// Handler が nil を返せば done、error を返せば指数バックオフでリスケ、
// 上限到達で failed とします（03-basic-design 4.3節）。
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// Handler Outboxイベントを1件処理する関数型。
// tx は Relay が Begin したトランザクション。Handler内でCommit/Rollbackはしない。
type Handler func(ctx context.Context, tx pgx.Tx, evt sqlc.FetchPendingOutboxRow) error

// Pool Begin可能なpgxプールのインターフェース（テスト差し替え用）
type Pool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Relay Outboxイベントを1バッチ分ずつ処理するワーカー
type Relay struct {
	Pool       Pool
	Queries    *sqlc.Queries
	Handler    Handler
	Logger     *slog.Logger
	Batch      int
	MaxRetries int
	Now        func() time.Time
	// Backoff retry_count(N回目のリトライ後)から次回実行時刻の相対を返します
	Backoff func(retryCount int) time.Duration
}

// DefaultBackoff 03-basic-designの規定: 2^(retry_count-1)分 (1,2,4,8,16,32)
func DefaultBackoff(retryCount int) time.Duration {
	if retryCount < 1 {
		retryCount = 1
	}
	minutes := 1
	for i := 1; i < retryCount; i++ {
		minutes *= 2
	}
	return time.Duration(minutes) * time.Minute
}

// New 既定値付きのRelayを構築
func New(pool Pool, q *sqlc.Queries, h Handler, logger *slog.Logger) *Relay {
	return &Relay{
		Pool: pool, Queries: q, Handler: h, Logger: logger,
		Batch: 10, MaxRetries: 6, Now: time.Now, Backoff: DefaultBackoff,
	}
}

// RunOnce 1バッチ分のイベントを処理して結果件数を返します。
// 呼び出し側で time.Tickerと組み合わせて定周期呼び出しにします。
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("outbox: Begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	qTx := r.Queries.WithTx(tx)
	batch := r.Batch
	if batch <= 0 {
		batch = 10
	}
	if batch > 1000 {
		batch = 1000
	}
	events, err := qTx.FetchPendingOutbox(ctx, int32(batch)) // #nosec G115 -- 上でクランプ済み
	if err != nil {
		return 0, fmt.Errorf("outbox: Fetch: %w", err)
	}
	if len(events) == 0 {
		return 0, nil
	}
	processed := 0
	for _, evt := range events {
		if err := r.handleOne(ctx, tx, qTx, evt); err != nil {
			r.Logger.Error("outbox: handle失敗", "id", evt.ID, "err", err)
			// 個別イベントの失敗ではバッチ全体は落とさない（既に個別に再スケジュール済み）
		} else {
			processed++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return processed, fmt.Errorf("outbox: Commit: %w", err)
	}
	return processed, nil
}

// handleOne 1イベント分の処理。Handlerの成否で done / reschedule / failed を分岐します。
func (r *Relay) handleOne(ctx context.Context, tx pgx.Tx, qTx *sqlc.Queries, evt sqlc.FetchPendingOutboxRow) error {
	err := r.Handler(ctx, tx, evt)
	if err == nil {
		if markErr := qTx.MarkOutboxDone(ctx, evt.ID); markErr != nil {
			return fmt.Errorf("MarkOutboxDone: %w", markErr)
		}
		return nil
	}
	retriesSoFar := int(evt.RetryCount) + 1
	if retriesSoFar >= r.MaxRetries {
		if markErr := qTx.MarkOutboxFailed(ctx, sqlc.MarkOutboxFailedParams{ID: evt.ID, LastError: pgTextFromErr(err)}); markErr != nil {
			return fmt.Errorf("MarkOutboxFailed: %w", markErr)
		}
		return err
	}
	next := r.Now().Add(r.Backoff(retriesSoFar))
	if reErr := qTx.RescheduleOutbox(ctx, sqlc.RescheduleOutboxParams{
		ID:            evt.ID,
		NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: true},
		LastError:     pgTextFromErr(err),
	}); reErr != nil {
		return fmt.Errorf("RescheduleOutbox: %w", reErr)
	}
	return err
}

func pgTextFromErr(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	if len(s) > 1000 {
		s = s[:1000]
	}
	return &s
}

// Loop RunOnceを定周期で回し続けます。ctx.Done() で終了します。
func (r *Relay) Loop(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := r.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.Logger.Error("outbox: RunOnce", "err", err)
			}
		}
	}
}
