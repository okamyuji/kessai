package ledger_test

import (
	"errors"
	"testing"

	"github.com/okamyuji/kessai/internal/ledger"
	"github.com/okamyuji/kessai/internal/platform/money"
)

func TestDeriveTransferID_Deterministic(t *testing.T) {
	t.Parallel()
	t1, err := ledger.DeriveTransferID("PAY01", ledger.EventCapture, 1)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	t2, _ := ledger.DeriveTransferID("PAY01", ledger.EventCapture, 1)
	if t1 != t2 {
		t.Fatalf("同じ入力から違うIDが生成された: %q vs %q", t1, t2)
	}
	if t1 != "PAY01:capture:1" {
		t.Fatalf("t1=%q", t1)
	}
}

func TestDeriveTransferID_Errors(t *testing.T) {
	t.Parallel()
	if _, err := ledger.DeriveTransferID("", ledger.EventCapture, 1); !errors.Is(err, ledger.ErrEmptyPaymentID) {
		t.Fatalf("空payment_idはErrEmptyPaymentID: %v", err)
	}
	if _, err := ledger.DeriveTransferID("PAY01", ledger.EventCapture, 0); err == nil {
		t.Fatalf("連番0はエラー")
	}
}

func TestBuildCaptureTransfer_AccountsAndSides(t *testing.T) {
	t.Parallel()
	tr, err := ledger.BuildCaptureTransfer("PAY01", 1, money.MustNew(1000))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if tr.Debit.Account != ledger.AccountPSPReceivable || tr.Debit.Side != ledger.SideDebit {
		t.Fatalf("capture debit不正: %+v", tr.Debit)
	}
	if tr.Credit.Account != ledger.AccountSales || tr.Credit.Side != ledger.SideCredit {
		t.Fatalf("capture credit不正: %+v", tr.Credit)
	}
	if !tr.Debit.Amount.Equal(tr.Credit.Amount) {
		t.Fatalf("借貸金額不一致")
	}
	if tr.Debit.TransferID != tr.Credit.TransferID || tr.Debit.TransferID != tr.ID {
		t.Fatalf("transfer_id不一致")
	}
}

func TestBuildRefundTransfer_AccountsAndSides(t *testing.T) {
	t.Parallel()
	tr, err := ledger.BuildRefundTransfer("PAY01", 1, money.MustNew(500))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if tr.Debit.Account != ledger.AccountRefunds || tr.Debit.Side != ledger.SideDebit {
		t.Fatalf("refund debit不正: %+v", tr.Debit)
	}
	if tr.Credit.Account != ledger.AccountPSPReceivable || tr.Credit.Side != ledger.SideCredit {
		t.Fatalf("refund credit不正: %+v", tr.Credit)
	}
}

func TestBuildTransfer_Errors(t *testing.T) {
	t.Parallel()
	if _, err := ledger.BuildCaptureTransfer("", 1, money.MustNew(1)); !errors.Is(err, ledger.ErrEmptyPaymentID) {
		t.Fatalf("empty payment_id")
	}
	if _, err := ledger.BuildCaptureTransfer("PAY01", 1, money.MustNew(0)); !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Fatalf("zero amount")
	}
	if _, err := ledger.BuildRefundTransfer("", 1, money.MustNew(1)); !errors.Is(err, ledger.ErrEmptyPaymentID) {
		t.Fatalf("refund empty payment_id")
	}
	if _, err := ledger.BuildRefundTransfer("PAY01", 1, money.MustNew(0)); !errors.Is(err, ledger.ErrInvalidAmount) {
		t.Fatalf("refund zero amount")
	}
	if _, err := ledger.BuildCaptureTransfer("PAY01", 0, money.MustNew(1)); err == nil {
		t.Fatalf("seq=0 は連番エラー")
	}
	if _, err := ledger.BuildRefundTransfer("PAY01", 0, money.MustNew(1)); err == nil {
		t.Fatalf("refund seq=0 は連番エラー")
	}
}

func TestInvariant_Balanced(t *testing.T) {
	t.Parallel()
	cap1, _ := ledger.BuildCaptureTransfer("PAY01", 1, money.MustNew(1000))
	ref1, _ := ledger.BuildRefundTransfer("PAY01", 1, money.MustNew(400))
	entries := []ledger.Entry{cap1.Debit, cap1.Credit, ref1.Debit, ref1.Credit}
	if err := ledger.Invariant(entries); err != nil {
		t.Fatalf("balancedがInvariantを通らない: %v", err)
	}
	// PSP未収金の残高は 1000（capture debit） - 400（refund credit）= 600
	bal, err := ledger.Balance(entries, ledger.AccountPSPReceivable)
	if err != nil {
		t.Fatalf("Balance err=%v", err)
	}
	if bal != 600 {
		t.Fatalf("PSP未収金残高=%d want 600", bal)
	}
	// 売上残高は貸方だけなので -1000
	if bal, _ := ledger.Balance(entries, ledger.AccountSales); bal != -1000 {
		t.Fatalf("売上残高=%d want -1000", bal)
	}
}

func TestInvariant_Unbalanced(t *testing.T) {
	t.Parallel()
	entries := []ledger.Entry{
		{TransferID: "T1", Account: ledger.AccountPSPReceivable, Side: ledger.SideDebit, Amount: money.MustNew(100), PaymentID: "P"},
		{TransferID: "T1", Account: ledger.AccountSales, Side: ledger.SideCredit, Amount: money.MustNew(90), PaymentID: "P"},
	}
	if err := ledger.Invariant(entries); err == nil {
		t.Fatalf("借貸不一致を検出すべき")
	}
}

func TestBalance_UnknownSide(t *testing.T) {
	t.Parallel()
	entries := []ledger.Entry{{TransferID: "T", Account: ledger.AccountSales, Side: "sideways", Amount: money.MustNew(1), PaymentID: "P"}}
	if _, err := ledger.Balance(entries, ledger.AccountSales); err == nil {
		t.Fatalf("未知sideを検出すべき")
	}
}

func TestAllEventKinds(t *testing.T) {
	t.Parallel()
	got := ledger.AllEventKinds()
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
}
