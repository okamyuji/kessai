// Package payment 決済のユースケース層です。
// idempotency層・stripeclient層・DB（sqlc生成）を組み合わせ、
// 03-basic-design 4.1節「決済実行フロー」を実装します。
package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/okamyuji/kessai/internal/payment/idempotency"
	"github.com/okamyuji/kessai/internal/payment/statemachine"
	"github.com/okamyuji/kessai/internal/payment/stripeclient"
	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/money"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// Store DBアクセスの薄い抽象。sqlcの*Queriesを実装として満たします（テストで差し替え可能）。
// トランザクションが必要な操作はStartTxで得た子Storeを使います。
type Store interface {
	Queries() *sqlc.Queries
	StartTx(ctx context.Context) (TxStore, error)
}

// TxStore トランザクション内でのStore。CommitまたはRollbackで終了します。
type TxStore interface {
	Queries() *sqlc.Queries
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// PGStore pgxpoolを保持しStoreを実装する具象型
type PGStore struct {
	pool interface { // pgx.Beginを持つ最小インターフェース。テスト差し替え可能。
		Begin(ctx context.Context) (pgx.Tx, error)
	}
	queries *sqlc.Queries
}

// NewPGStore pgxのプールから*PGStoreを作る。プールは*pgxpool.Poolが実装済み。
func NewPGStore(pool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}, q *sqlc.Queries) *PGStore {
	return &PGStore{pool: pool, queries: q}
}

// Queries トップレベル（トランザクション外）のQueries
func (s *PGStore) Queries() *sqlc.Queries { return s.queries }

// StartTx 新規トランザクションを開始
func (s *PGStore) StartTx(ctx context.Context) (TxStore, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("payment: BeginTx: %w", err)
	}
	return &pgTxStore{tx: tx, q: s.queries.WithTx(tx)}, nil
}

type pgTxStore struct {
	tx pgx.Tx
	q  *sqlc.Queries
}

func (t *pgTxStore) Queries() *sqlc.Queries             { return t.q }
func (t *pgTxStore) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *pgTxStore) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// StartCheckoutRequest 顧客がチェックアウトを開始したリクエスト
type StartCheckoutRequest struct {
	IdempotencyKey string // ULIDまたはそれに準じる文字列。空なら呼び出し側で生成される想定
	ProductID      string
	Amount         money.JPY
	CaptureMode    string // "manual" / "auto"
	Actor          string // 監査ログ用（未認証顧客の場合は "customer"）
}

// StartCheckoutResult チェックアウト開始の結果
type StartCheckoutResult struct {
	PaymentID    string
	ClientSecret string
	Status       string
}

// ErrIdempotencyConflict 同一キーで異なる本文
var ErrIdempotencyConflict = errors.New("payment: idempotency key の本文が既存と異なる")

// ErrIdempotencyInProgress 同一キーの先行処理が未確定
var ErrIdempotencyInProgress = errors.New("payment: idempotency key の先行処理が未確定")

// UseCase 決済ユースケース
type UseCase struct {
	store          Store
	stripe         stripeclient.Client
	ids            idgen.Generator
	captureMode    string
	idempotencyTTL time.Duration
	now            func() time.Time
}

// NewUseCase 依存を受け取ってUseCaseを構築します
func NewUseCase(store Store, sc stripeclient.Client, ids idgen.Generator, captureMode string, idempotencyTTL time.Duration) *UseCase {
	return &UseCase{
		store: store, stripe: sc, ids: ids,
		captureMode: captureMode, idempotencyTTL: idempotencyTTL,
		now: time.Now,
	}
}

// StartCheckout 03-basic-design 4.1のフロー実装:
//  1. 冪等性キーがあれば既存レスポンスを再現
//  2. 同一トランザクションでpayments/idempotency_keysをINSERT
//  3. Stripe PaymentIntent作成を同期呼び出し（Idempotency-Key付き）
//  4. response_snapshot と stripe_payment_intent_id を保存してコミット
func (u *UseCase) StartCheckout(ctx context.Context, req StartCheckoutRequest) (*StartCheckoutResult, error) {
	if err := u.validateCheckout(&req); err != nil {
		return nil, err
	}
	reqHash := computeReqHash(req)
	if replay, err := u.tryIdempotentReplay(ctx, req.IdempotencyKey, reqHash); err != nil || replay != nil {
		return replay, err
	}
	paymentID, err := u.reserveCheckoutSlot(ctx, req, reqHash)
	if err != nil {
		return nil, err
	}
	intent, err := u.stripe.CreatePaymentIntent(ctx, stripeclient.CreateIntentRequest{
		IdempotencyMaster: req.IdempotencyKey,
		Amount:            req.Amount,
		CaptureMode:       req.CaptureMode,
		Description:       fmt.Sprintf("product=%s", req.ProductID),
	})
	if err != nil {
		return nil, err
	}
	return u.persistCheckoutResult(ctx, req.IdempotencyKey, paymentID, intent)
}

func (u *UseCase) validateCheckout(req *StartCheckoutRequest) error {
	if req.IdempotencyKey == "" {
		return errors.New("payment: idempotency_key必須")
	}
	if req.ProductID == "" {
		return errors.New("payment: product_id必須")
	}
	if req.Amount.Int64() <= 0 {
		return errors.New("payment: amountは正の整数")
	}
	if req.CaptureMode == "" {
		req.CaptureMode = u.captureMode
	}
	if req.CaptureMode != "manual" && req.CaptureMode != "auto" {
		return errors.New("payment: capture_modeはmanual/autoのいずれか")
	}
	return nil
}

// tryIdempotentReplay 既存のidempotency_keys行があれば結果を返します。無ければ(nil, nil)。
func (u *UseCase) tryIdempotentReplay(ctx context.Context, key string, reqHash []byte) (*StartCheckoutResult, error) {
	existing, err := u.store.Queries().GetIdempotency(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("payment: GetIdempotency: %w", err)
	}
	if !idempotency.EqualHash(existing.RequestHash, reqHash) {
		return nil, ErrIdempotencyConflict
	}
	if existing.ResponseSnapshot == nil {
		return nil, ErrIdempotencyInProgress
	}
	return decodeSnapshot(existing.ResponseSnapshot)
}

// reserveCheckoutSlot payments+idempotency_keysを同一トランザクションでINSERTし、payment_idを返します。
// 並行で先行リクエストが挿入済みならErrIdempotencyInProgress。
func (u *UseCase) reserveCheckoutSlot(ctx context.Context, req StartCheckoutRequest, reqHash []byte) (string, error) {
	tx, err := u.store.StartTx(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	paymentID := u.ids.New()
	if _, err := tx.Queries().CreatePayment(ctx, sqlc.CreatePaymentParams{
		ID:        paymentID,
		ProductID: req.ProductID,
		AmountJpy: req.Amount.Int64(),
	}); err != nil {
		return "", fmt.Errorf("payment: CreatePayment: %w", err)
	}
	expiresAt := u.now().Add(u.idempotencyTTL)
	ins, err := tx.Queries().TryInsertIdempotency(ctx, sqlc.TryInsertIdempotencyParams{
		Key:         req.IdempotencyKey,
		RequestHash: reqHash,
		PaymentID:   &paymentID,
		ExpiresAt:   pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		// ON CONFLICT DO NOTHING で挿入されなかった場合、
		// RETURNING 節は0行を返し、pgx側は pgx.ErrNoRows を返す
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrIdempotencyInProgress
		}
		return "", fmt.Errorf("payment: TryInsertIdempotency: %w", err)
	}
	if ins.Key == "" {
		return "", ErrIdempotencyInProgress
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("payment: Commit: %w", err)
	}
	return paymentID, nil
}

// persistCheckoutResult Stripeの応答をpaymentsとidempotency_keysへ反映します
func (u *UseCase) persistCheckoutResult(ctx context.Context, key, paymentID string, intent *stripeclient.Intent) (*StartCheckoutResult, error) {
	result := &StartCheckoutResult{
		PaymentID:    paymentID,
		ClientSecret: intent.ClientSecret,
		Status:       intent.Status,
	}
	// スナップショットは idempotency_keys.response_snapshot に保存。
	// 内容にClientSecretを含めるのは同一冪等キー再送時に返却するため（Stripe側の仕組みと同等）。
	// このカラムを他所へ出力しない限り漏洩リスクはない。
	snap, err := json.Marshal(result) // #nosec G117 -- ClientSecretは意図的にスナップショット化する
	if err != nil {
		return nil, fmt.Errorf("payment: snapshot: %w", err)
	}
	if err := u.store.Queries().SetStripePaymentIntent(ctx, sqlc.SetStripePaymentIntentParams{
		ID: paymentID, StripePaymentIntentID: &intent.ID,
	}); err != nil {
		return nil, fmt.Errorf("payment: SetStripePaymentIntent: %w", err)
	}
	if err := u.store.Queries().SetIdempotencyResponse(ctx, sqlc.SetIdempotencyResponseParams{
		Key: key, ResponseSnapshot: snap, PaymentID: &paymentID,
	}); err != nil {
		return nil, fmt.Errorf("payment: SetIdempotencyResponse: %w", err)
	}
	return result, nil
}

// ApplyEvent 状態遷移をテーブル駆動で適用し、payment_transitionsへ追記します。
// Outboxリレー・Webhook処理から呼ばれる想定。DBトランザクションは呼び出し側で管理してください。
func (u *UseCase) ApplyEvent(ctx context.Context, tx TxStore, paymentID, actor string, event statemachine.Event) (statemachine.State, error) {
	p, err := tx.Queries().GetPaymentForUpdate(ctx, paymentID)
	if err != nil {
		return "", fmt.Errorf("payment: GetPaymentForUpdate: %w", err)
	}
	from := statemachine.State(p.State)
	to, err := statemachine.Next(from, event)
	if err != nil {
		return "", err
	}
	upd, err := tx.Queries().UpdatePaymentState(ctx, sqlc.UpdatePaymentStateParams{
		ID:      paymentID,
		State:   sqlc.PaymentState(to),
		State_2: sqlc.PaymentState(from),
	})
	if err != nil {
		return "", fmt.Errorf("payment: UpdatePaymentState: %w", err)
	}
	if err := tx.Queries().InsertPaymentTransition(ctx, sqlc.InsertPaymentTransitionParams{
		ID:        u.ids.New(),
		PaymentID: paymentID,
		FromState: sqlc.PaymentState(from),
		ToState:   sqlc.PaymentState(to),
		Event:     string(event),
		Actor:     actor,
	}); err != nil {
		return "", fmt.Errorf("payment: InsertPaymentTransition: %w", err)
	}
	_ = upd // 現状は状態のみを返すが将来的にupd値を使う余地を残す
	return to, nil
}

// IsPGDuplicateError PostgreSQLの一意制約違反かどうか
func IsPGDuplicateError(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	return pgErr.Code == "23505"
}

func computeReqHash(r StartCheckoutRequest) []byte {
	body, _ := json.Marshal(struct {
		P string `json:"p"`
		A int64  `json:"a"`
		C string `json:"c"`
	}{P: r.ProductID, A: r.Amount.Int64(), C: r.CaptureMode})
	return idempotency.HashRequest(body)
}

func decodeSnapshot(raw []byte) (*StartCheckoutResult, error) {
	var out StartCheckoutResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("payment: decodeSnapshot: %w", err)
	}
	return &out, nil
}
