// Package ledger 複式簿記台帳のドメインロジックを提供します（ADR-0008）。
// エントリは追記のみで、1取引を借方・貸方の2行1組で表現します。
// 勘定科目はMVPで psp_receivable / sales / refunds の3種です。
// 手数料・入金の勘定は第3段階の対帳導入時に追加します。
package ledger

import (
	"errors"
	"fmt"

	"github.com/okamyuji/kessai/internal/platform/money"
)

// Account 勘定科目
type Account string

const (
	// AccountPSPReceivable PSP未収金
	AccountPSPReceivable Account = "psp_receivable"
	// AccountSales 売上
	AccountSales Account = "sales"
	// AccountRefunds 返金
	AccountRefunds Account = "refunds"
)

// Side 借方・貸方
type Side string

const (
	// SideDebit 借方
	SideDebit Side = "debit"
	// SideCredit 貸方
	SideCredit Side = "credit"
)

// Entry 台帳の1行
type Entry struct {
	TransferID string
	Account    Account
	Side       Side
	Amount     money.JPY
	PaymentID  string
}

// Transfer 借方・貸方の対（1取引 = 2エントリ）
type Transfer struct {
	ID     string
	Debit  Entry
	Credit Entry
}

// EventKind 台帳を動かすイベント種別
type EventKind string

const (
	// EventCapture 売上確定（PSP未収金/売上）
	EventCapture EventKind = "capture"
	// EventRefund 返金（返金/PSP未収金）
	EventRefund EventKind = "refund"
)

// AllEventKinds 台帳イベント全種別（テスト網羅用）
func AllEventKinds() []EventKind { return []EventKind{EventCapture, EventRefund} }

// ErrInvalidAmount 金額がゼロ
var ErrInvalidAmount = errors.New("ledger: 金額はゼロより大きくなければならない")

// ErrEmptyPaymentID payment_idが空
var ErrEmptyPaymentID = errors.New("ledger: payment_idは空にできない")

// ErrUnknownEvent 未対応イベント
var ErrUnknownEvent = errors.New("ledger: 未対応のイベント")

// DeriveTransferID payment_id / イベント種別 / 連番から決定的にtransfer_idを導出します
// Outboxリトライで同じ入力から必ず同じtransfer_idが得られるため、UNIQUE(transfer_id, side)による二重記帳防止が働きます
func DeriveTransferID(paymentID string, event EventKind, seq int) (string, error) {
	if paymentID == "" {
		return "", ErrEmptyPaymentID
	}
	if seq < 1 {
		return "", fmt.Errorf("ledger: 連番は1以上: got %d", seq)
	}
	return fmt.Sprintf("%s:%s:%d", paymentID, event, seq), nil
}

// BuildCaptureTransfer キャプチャ時の借方=PSP未収金 / 貸方=売上
func BuildCaptureTransfer(paymentID string, seq int, amount money.JPY) (Transfer, error) {
	if err := validateCommon(paymentID, amount); err != nil {
		return Transfer{}, err
	}
	tid, err := DeriveTransferID(paymentID, EventCapture, seq)
	if err != nil {
		return Transfer{}, err
	}
	return Transfer{
		ID: tid,
		Debit: Entry{
			TransferID: tid, Account: AccountPSPReceivable, Side: SideDebit,
			Amount: amount, PaymentID: paymentID,
		},
		Credit: Entry{
			TransferID: tid, Account: AccountSales, Side: SideCredit,
			Amount: amount, PaymentID: paymentID,
		},
	}, nil
}

// BuildRefundTransfer 返金時の借方=返金 / 貸方=PSP未収金
func BuildRefundTransfer(paymentID string, seq int, amount money.JPY) (Transfer, error) {
	if err := validateCommon(paymentID, amount); err != nil {
		return Transfer{}, err
	}
	tid, err := DeriveTransferID(paymentID, EventRefund, seq)
	if err != nil {
		return Transfer{}, err
	}
	return Transfer{
		ID: tid,
		Debit: Entry{
			TransferID: tid, Account: AccountRefunds, Side: SideDebit,
			Amount: amount, PaymentID: paymentID,
		},
		Credit: Entry{
			TransferID: tid, Account: AccountPSPReceivable, Side: SideCredit,
			Amount: amount, PaymentID: paymentID,
		},
	}, nil
}

// Balance 勘定ごとの借方合計 - 貸方合計を返します
func Balance(entries []Entry, account Account) (int64, error) {
	var debit, credit int64
	for _, e := range entries {
		if e.Account != account {
			continue
		}
		switch e.Side {
		case SideDebit:
			debit += e.Amount.Int64()
		case SideCredit:
			credit += e.Amount.Int64()
		default:
			return 0, fmt.Errorf("ledger: 不明なside %q", e.Side)
		}
	}
	return debit - credit, nil
}

// Invariant transfer_id単位で借方合計=貸方合計が成立するかを検証します（NFR-1）
func Invariant(entries []Entry) error {
	sumByTransfer := map[string][2]int64{} // [0]=debit, [1]=credit
	for _, e := range entries {
		sums := sumByTransfer[e.TransferID]
		switch e.Side {
		case SideDebit:
			sums[0] += e.Amount.Int64()
		case SideCredit:
			sums[1] += e.Amount.Int64()
		default:
			return fmt.Errorf("ledger: 不明なside %q", e.Side)
		}
		sumByTransfer[e.TransferID] = sums
	}
	for tid, sums := range sumByTransfer {
		if sums[0] != sums[1] {
			return fmt.Errorf("ledger: 借貸不一致 transfer_id=%s debit=%d credit=%d", tid, sums[0], sums[1])
		}
	}
	return nil
}

func validateCommon(paymentID string, amount money.JPY) error {
	if paymentID == "" {
		return ErrEmptyPaymentID
	}
	if amount.Int64() <= 0 {
		return ErrInvalidAmount
	}
	return nil
}
