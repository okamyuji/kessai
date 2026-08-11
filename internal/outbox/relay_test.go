package outbox_test

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okamyuji/kessai/internal/outbox"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/testsupport/pgcontainer"
)

var sharedPG *pgcontainer.Container

func TestMain(m *testing.M) {
	if os.Getenv("KESSAI_SKIP_INTEGRATION") != "" {
		os.Exit(m.Run())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := pgcontainer.Start(ctx)
	if err != nil {
		log.Printf("pgcontainer未起動でスキップ: %v", err)
		os.Exit(m.Run())
	}
	sharedPG = c
	code := m.Run()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	_ = sharedPG.Stop(stopCtx)
	os.Exit(code)
}

func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if sharedPG == nil {
		t.Skip("testcontainers未起動のためスキップ")
	}
	if err := sharedPG.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return sharedPG.Pool()
}

func enqueue(t *testing.T, ctx context.Context, q *sqlc.Queries, eventType string) string {
	t.Helper()
	ids := idgen.NewDefault()
	id := ids.New()
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		ID: id, EventType: eventType, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return id
}

func TestRelay_RunOnce_HappyPath_MarksDone(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	enqueue(t, ctx, q, "test.ok")
	enqueue(t, ctx, q, "test.ok")

	handler := func(_ context.Context, _ pgx.Tx, _ sqlc.FetchPendingOutboxRow) error { return nil }
	r := outbox.New(pool, q, handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("processed=%d want 2", n)
	}
	// 全部doneになっていて、pendingは残らない
	rows, err := q.ListOutboxEvents(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r.Status != sqlc.OutboxStatusDone {
			t.Fatalf("status=%s want done: %+v", r.Status, r)
		}
	}
}

func TestRelay_HandlerFails_Reschedules(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	id := enqueue(t, ctx, q, "test.fail")

	fixedNow := time.Now()
	handler := func(_ context.Context, _ pgx.Tx, _ sqlc.FetchPendingOutboxRow) error {
		return errors.New("boom")
	}
	r := outbox.New(pool, q, handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Now = func() time.Time { return fixedNow }
	r.Backoff = func(n int) time.Duration { return time.Duration(n) * time.Minute }

	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rows, _ := q.ListOutboxEvents(ctx, 10)
	if len(rows) != 1 {
		t.Fatalf("len=%d", len(rows))
	}
	got := rows[0]
	if got.ID != id {
		t.Fatalf("id mismatch")
	}
	if got.Status != sqlc.OutboxStatusPending {
		t.Fatalf("status=%s want pending", got.Status)
	}
	if got.RetryCount != 1 {
		t.Fatalf("retry=%d want 1", got.RetryCount)
	}
	// 次回実行時刻が1分後
	if !got.NextAttemptAt.Time.After(fixedNow) {
		t.Fatalf("next_attempt_at should be in future: %v", got.NextAttemptAt.Time)
	}
}

func TestRelay_HandlerFailsUntilMax_MarksFailed(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	enqueue(t, ctx, q, "test.retryexhaust")

	handler := func(_ context.Context, _ pgx.Tx, _ sqlc.FetchPendingOutboxRow) error {
		return errors.New("always fail")
	}
	r := outbox.New(pool, q, handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.MaxRetries = 2                                 // MaxRetries=2でRunOnceを2回呼べば failed になる
	r.Backoff = func(int) time.Duration { return 0 } // 即再取得可能に

	for range 3 {
		_, _ = r.RunOnce(ctx)
	}
	rows, _ := q.ListOutboxEvents(ctx, 10)
	if len(rows) != 1 {
		t.Fatalf("len=%d", len(rows))
	}
	if rows[0].Status != sqlc.OutboxStatusFailed {
		t.Fatalf("status=%s want failed", rows[0].Status)
	}
	if rows[0].LastError == nil || *rows[0].LastError == "" {
		t.Fatalf("last_error 未設定")
	}
}

func TestLoop_StopsOnContextCancel(t *testing.T) {
	pool := requirePG(t)
	ctx, cancel := context.WithCancel(context.Background())
	q := sqlc.New(pool)
	enqueue(t, ctx, q, "test.loop")
	var seen int
	handler := func(_ context.Context, _ pgx.Tx, _ sqlc.FetchPendingOutboxRow) error {
		seen++
		return nil
	}
	r := outbox.New(pool, q, handler, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() {
		r.Loop(ctx, 10*time.Millisecond)
		close(done)
	}()
	// 1回以上RunOnceが走るのを待ってからキャンセル
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Loopがctx.Done()で終了しない")
	}
	if seen < 1 {
		t.Fatalf("Loop中にhandlerが呼ばれていない: seen=%d", seen)
	}
}

func TestDefaultBackoff_Doubles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		want time.Duration
	}{
		{0, time.Minute},
		{1, time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{6, 32 * time.Minute},
	}
	for _, c := range cases {
		if got := outbox.DefaultBackoff(c.n); got != c.want {
			t.Errorf("DefaultBackoff(%d)=%v want %v", c.n, got, c.want)
		}
	}
}
