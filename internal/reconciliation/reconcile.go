// Package reconciliation StripeのBalance Transactionと自社の台帳を突き合わせて差分を検出します（FR-B3、03-basic-design 9章）。
// 現段階は対帳の簡易版（金額合計の突合）で、詳細な入金明細対応は本番運用時に拡張します。
package reconciliation

import (
	"context"
	"fmt"

	"github.com/okamyuji/kessai/internal/ledger"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// StripeStatement Stripe側の入金合計（Balance Transaction API等から取得する想定）
type StripeStatement struct {
	NetJPY int64 // 手数料控除後の入金額
}

// Reconciler 対帳の実行器
type Reconciler struct {
	Queries *sqlc.Queries
}

// New Reconcilerを構築します
func New(q *sqlc.Queries) *Reconciler { return &Reconciler{Queries: q} }

// Result 対帳結果
type Result struct {
	LedgerNetJPY  int64
	StripeNetJPY  int64
	DifferenceJPY int64 // Ledger - Stripe（正なら自社台帳の方が多い＝Stripe明細に未反映の売上あり）
	Balanced      bool
}

// Reconcile PSP未収金の残高（借-貸）とStripe側入金合計を突き合わせます。
// 完全一致が理想ですが、現段階は差分を返して呼び出し側に判断を委ねます。
func (r *Reconciler) Reconcile(ctx context.Context, stripe StripeStatement) (Result, error) {
	sum, err := r.Queries.SumLedgerBalance(ctx, sqlc.LedgerAccountPspReceivable)
	if err != nil {
		return Result{}, fmt.Errorf("reconciliation: SumLedgerBalance: %w", err)
	}
	ledgerNet := sum.DebitTotal - sum.CreditTotal
	diff := ledgerNet - stripe.NetJPY
	return Result{
		LedgerNetJPY:  ledgerNet,
		StripeNetJPY:  stripe.NetJPY,
		DifferenceJPY: diff,
		Balanced:      diff == 0,
	}, nil
}

// ExpectedAccount 対帳対象の勘定科目（現状はPSP未収金のみ）
func ExpectedAccount() ledger.Account { return ledger.AccountPSPReceivable }
