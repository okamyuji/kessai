// sqlc生成のQueriesを実PostgreSQL（testcontainers共有コンテナ）で検証します。
// モック・スタブは使いません。DBの制約や句（SKIP LOCKED、ON CONFLICT）がSQL側で
// 期待どおり働くかを確認する層です。
package sqlc_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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

func seedProductAndPayment(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (paymentID string) {
	t.Helper()
	ids := idgen.NewDefault()
	productID := ids.New()
	paymentID = ids.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		productID, "seed", int64(1000), `{}`); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, product_id, amount_jpy) VALUES ($1,$2,$3)`,
		paymentID, productID, int64(1000)); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return paymentID
}

// TryInsertIdempotency 二重INSERTでON CONFLICT DO NOTHINGが働き、Key=""（0行）を返すこと
func TestTryInsertIdempotency_ConflictReturnsEmpty(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	payID := seedProductAndPayment(t, ctx, pool)
	q := sqlc.New(pool)
	key := idgen.NewDefault().New()
	params := sqlc.TryInsertIdempotencyParams{
		Key:         key,
		RequestHash: []byte{1, 2, 3, 4},
		PaymentID:   &payID,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}
	first, err := q.TryInsertIdempotency(ctx, params)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if first.Key != key {
		t.Fatalf("first.Key=%q want %q", first.Key, key)
	}
	// sqlcは `ON CONFLICT DO NOTHING ... RETURNING` の0行時にpgx.ErrNoRowsを返す
	_, err = q.TryInsertIdempotency(ctx, params)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("競合時はpgx.ErrNoRowsを返すべき: got %v", err)
	}
}

// SetIdempotencyResponse 後からsnapshotを埋め、GetIdempotencyで戻ること
func TestSetIdempotencyResponse_UpdatesSnapshot(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	payID := seedProductAndPayment(t, ctx, pool)
	q := sqlc.New(pool)
	key := idgen.NewDefault().New()
	if _, err := q.TryInsertIdempotency(ctx, sqlc.TryInsertIdempotencyParams{
		Key: key, RequestHash: []byte{9}, PaymentID: &payID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	snap := []byte(`{"ok":true}`)
	if err := q.SetIdempotencyResponse(ctx, sqlc.SetIdempotencyResponseParams{
		Key: key, ResponseSnapshot: snap, PaymentID: &payID,
	}); err != nil {
		t.Fatalf("SetIdempotencyResponse: %v", err)
	}
	got, err := q.GetIdempotency(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// PostgreSQL JSONBは正規化して保存するため、生バイト比較ではなくJSONとして意味的に比較する
	var wantMap, gotMap map[string]any
	if err := json.Unmarshal(snap, &wantMap); err != nil {
		t.Fatalf("wantMap: %v", err)
	}
	if err := json.Unmarshal(got.ResponseSnapshot, &gotMap); err != nil {
		t.Fatalf("gotMap: %v", err)
	}
	if gotMap["ok"] != wantMap["ok"] {
		t.Fatalf("snapshot意味的不一致: got=%v want=%v", gotMap, wantMap)
	}
}

// InsertLedgerEntry ON CONFLICT (transfer_id, side) DO NOTHINGで二重記帳が抑止されること
func TestInsertLedgerEntry_ConflictSuppressesDuplicate(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	payID := seedProductAndPayment(t, ctx, pool)
	q := sqlc.New(pool)
	ids := idgen.NewDefault()
	params := sqlc.InsertLedgerEntryParams{
		ID:         ids.New(),
		TransferID: payID + ":capture:1",
		Account:    sqlc.LedgerAccountPspReceivable,
		Side:       sqlc.LedgerSideDebit,
		AmountJpy:  1000,
		PaymentID:  payID,
	}
	if err := q.InsertLedgerEntry(ctx, params); err != nil {
		t.Fatalf("1回目: %v", err)
	}
	// 同じtransfer_id+sideで再実行 → DO NOTHING（エラーにならず、行は増えない）
	params2 := params
	params2.ID = ids.New()
	if err := q.InsertLedgerEntry(ctx, params2); err != nil {
		t.Fatalf("2回目: %v", err)
	}
	rows, err := q.ListLedgerByPayment(ctx, payID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("台帳行数=%d want 1", len(rows))
	}
}

// FetchPendingOutbox 並行呼び出しでSKIP LOCKEDにより同一行を1ワーカーだけが取得すること
func TestFetchPendingOutbox_SkipLockedNoDouble(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	ids := idgen.NewDefault()
	// 3件 pendingを積む
	for range 3 {
		if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
			ID: ids.New(), EventType: "test.event", Payload: []byte(`{}`),
		}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	// 2つのトランザクションから同時にFetch。同じ行が両方に返らないこと。
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		seen        = map[string]int{}
		results     []int
		captureFail bool
	)
	worker := func() {
		defer wg.Done()
		tx, err := pool.Begin(ctx)
		if err != nil {
			mu.Lock()
			captureFail = true
			mu.Unlock()
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		qq := q.WithTx(tx)
		got, err := qq.FetchPendingOutbox(ctx, 5)
		if err != nil {
			mu.Lock()
			captureFail = true
			mu.Unlock()
			return
		}
		mu.Lock()
		results = append(results, len(got))
		for _, e := range got {
			seen[e.ID]++
		}
		mu.Unlock()
		// トランザクションを終わらせずに待つと相手側は空を返すはず。
		// 実際には両ワーカーが並列に走るのを再現するため一瞬待つ。
		time.Sleep(200 * time.Millisecond)
	}
	wg.Add(2)
	go worker()
	go worker()
	wg.Wait()
	if captureFail {
		t.Fatalf("worker失敗")
	}
	// 二重取得なし
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("SKIP LOCKEDが効いていない: %s x %d", id, n)
		}
	}
}
