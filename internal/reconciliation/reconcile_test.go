package reconciliation_test

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
	"github.com/okamyuji/kessai/internal/reconciliation"
	"github.com/okamyuji/kessai/internal/testsupport/pgcontainer"
)

var sharedPG *pgcontainer.Container

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := pgcontainer.Start(ctx)
	if err != nil {
		log.Printf("pgcontainer未起動: %v", err)
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
		t.Skip("testcontainers未起動")
	}
	if err := sharedPG.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	return sharedPG.Pool()
}

func seed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, captureJPY, refundJPY int64) {
	t.Helper()
	ids := idgen.NewDefault()
	prodID := ids.New()
	payID := ids.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO products (id, name, price_jpy, tokusho_snapshot) VALUES ($1,$2,$3,$4::jsonb)`,
		prodID, "demo", captureJPY, `{}`); err != nil {
		t.Fatalf("seed product: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO payments (id, product_id, amount_jpy) VALUES ($1,$2,$3)`,
		payID, prodID, captureJPY); err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	// capture: PSP未収金/売上 の借貸ペア
	if _, err := pool.Exec(ctx,
		`INSERT INTO ledger_entries (id, transfer_id, account, side, amount_jpy, payment_id) VALUES ($1,$2,'psp_receivable','debit',$3,$4)`,
		ids.New(), payID+":capture:1", captureJPY, payID); err != nil {
		t.Fatalf("seed debit: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO ledger_entries (id, transfer_id, account, side, amount_jpy, payment_id) VALUES ($1,$2,'sales','credit',$3,$4)`,
		ids.New(), payID+":capture:1", captureJPY, payID); err != nil {
		t.Fatalf("seed credit: %v", err)
	}
	if refundJPY > 0 {
		// refund: 返金/PSP未収金
		if _, err := pool.Exec(ctx,
			`INSERT INTO ledger_entries (id, transfer_id, account, side, amount_jpy, payment_id) VALUES ($1,$2,'refunds','debit',$3,$4)`,
			ids.New(), payID+":refund:1", refundJPY, payID); err != nil {
			t.Fatalf("seed refund debit: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO ledger_entries (id, transfer_id, account, side, amount_jpy, payment_id) VALUES ($1,$2,'psp_receivable','credit',$3,$4)`,
			ids.New(), payID+":refund:1", refundJPY, payID); err != nil {
			t.Fatalf("seed refund credit: %v", err)
		}
	}
}

func TestReconcile_Balanced(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	// 売上1000、返金なし → PSP未収金の残高=1000。Stripe側も1000ならBalanced
	seed(t, ctx, pool, 1000, 0)
	r := reconciliation.New(q)
	res, err := r.Reconcile(ctx, reconciliation.StripeStatement{NetJPY: 1000})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Balanced {
		t.Fatalf("Balanced want true: %+v", res)
	}
}

func TestReconcile_WithRefund(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	// 売上1000、返金400 → PSP未収金=600
	seed(t, ctx, pool, 1000, 400)
	r := reconciliation.New(q)
	res, err := r.Reconcile(ctx, reconciliation.StripeStatement{NetJPY: 600})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.Balanced {
		t.Fatalf("Balanced want true: %+v", res)
	}
}

func TestReconcile_Difference(t *testing.T) {
	pool := requirePG(t)
	ctx := context.Background()
	q := sqlc.New(pool)
	seed(t, ctx, pool, 1000, 0)
	r := reconciliation.New(q)
	res, _ := r.Reconcile(ctx, reconciliation.StripeStatement{NetJPY: 900})
	if res.Balanced {
		t.Fatalf("Balanced want false")
	}
	if res.DifferenceJPY != 100 {
		t.Fatalf("diff=%d want 100", res.DifferenceJPY)
	}
}
