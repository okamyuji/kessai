# kessai 基本設計書

作成日 2026-08-11 ／ 版 1.0

本書の各節は02-requirements-specificationのFR/NFR番号を参照します。設計上の決定の根拠はADR-0001〜0016にあります。

## 1. アーキテクチャ

モジュラーモノリス+単一PostgreSQLの構成です（ADR-0003、ADR-0016）。

```text
ブラウザ
  ├─ 購入ページ・最終確認画面（templ描画。Stripeトークン化JSを埋め込み） [FR-A1, FR-D2]
  └─ 管理画面（htmx + Tailwind。SSEでイベントログ自動更新）           [FR-C1〜C3]

Goアプリ（単一バイナリ、net/http + templ）
  ├─ internal/payment   決済ユースケース・状態遷移テーブル・Stripeクライアント
  │                      （Circuit Breaker内蔵） [FR-A1〜A4, FR-B1]
  ├─ internal/ledger    複式簿記台帳・残高導出・不変条件検証 [FR-D5, NFR-1]
  ├─ internal/webhook   署名検証・イベント記録・即時200応答 [FR-B2]
  ├─ internal/outbox       Outboxリレーワーカー（SKIP LOCKED取得・指数バックオフ） [NFR-2, NFR-3]
  ├─ internal/housekeeping 定期ジョブ（オーソリ・3DS未完了の失効、冪等性キー削除） [FR-A2, FR-A4]
  ├─ internal/admin        管理画面ハンドラ・認証・監査ログ [FR-C1〜C3, FR-D3, FR-D5]
  └─ internal/platform     設定読み込み・シークレット検証・ロガー・SSEハブ [FR-D4, FR-C3]

PostgreSQL 18（Docker Compose） [ADR-0015]
Stripeサンドボックス（Webhook はStripe CLIでローカル転送）
```

パッケージ間の依存は`platform ← 各ドメイン ← cmd/server`の一方向とし、ドメイン間はDBのトランザクション境界（同一`*pgxpool.Pool`）だけを共有します。

## 2. 状態遷移テーブル

### 2.1 決済の状態遷移（ADR-0011、FR-A2・B1）

状態×イベントの許可遷移だけを表にします。表にない組み合わせはすべて`invalid-transition`エラーです（ADR-0012）。

| 現在状態 \ イベント | AuthorizeSucceeded | AuthorizeFailed | Capture | Cancel | Expire | RefundPartial | RefundFull |
|---|---|---|---|---|---|---|---|
| created | authorized | failed | - | - | expired | - | - |
| authorized | - | - | captured | canceled | expired | - | - |
| captured | - | - | - | - | - | partially_refunded | refunded |
| partially_refunded | - | - | - | - | - | partially_refunded | refunded |
| failed / canceled / expired / refunded | - | - | - | - | - | - | - |

- 終端状態はfailed、canceled、expired、refundedの4つです。全状態が到達可能で、非終端状態は必ず脱出辺を持ちます。
- `RefundFull`は返金累計が取引金額に到達するイベントです。超過はイベント発火前に拒否します（FR-B1）。キャプチャは全額のみで、部分キャプチャはスコープ外です（FR-A2）。
- `created`の`Expire`は、3DS認証を途中離脱した等でオーソリ結果が届かないまま設定値`checkout_expiry_minutes`（既定60分）を過ぎた取引を`internal/housekeeping`が失効させ、あわせてStripeのPaymentIntentをキャンセルします。
- `authorized`の`Expire`はオーソリ有効期限を過ぎた取引を同ジョブが遷移させます。有効期限は設定値`auth_expiry_days`（既定21日）とし、Stripeの実際の期限より短く設定して先に自社側で失効させます。カードブランド側の期限短縮（2026年7月・国内25日という解説記事、未確認）は実装時にStripeの一次情報で確認して既定値を見直します。
- `failed`は終端です。Stripeでは失敗後に同一PaymentIntentで顧客が再試行できますが、本設計では`AuthorizeFailed`受信時にPaymentIntentをキャンセルし、再試行は新しい決済（新しい`payments`行と新しいPaymentIntent）として扱います。1つの`payments`行と1つのPaymentIntentを1対1に保つことで、failed後に成功Webhookが届く競合を仕組みで排除します。

### 2.1.1 Stripe Webhookイベントと遷移イベントの対応

| Stripeイベント種別 | 遷移イベント | 補足 |
|---|---|---|
| payment_intent.amount_capturable_updated | AuthorizeSucceeded | 手動キャプチャ（`CAPTURE_MODE=manual`）でのオーソリ完了 |
| payment_intent.succeeded | Capture（`created`からの場合はAuthorizeSucceededとCaptureの連続適用） | 自動キャプチャ時はこのイベントのみが届きます |
| payment_intent.payment_failed | AuthorizeFailed | 受信後にPaymentIntentをキャンセルします |
| payment_intent.canceled | Cancel | 自社起点のキャンセル・失効の完了確認 |
| charge.refunded | RefundPartial / RefundFull | 返金累計と取引金額の比較で判定します |

### 2.2 Circuit Breakerの状態遷移（ADR-0010、NFR-2）

| 現在状態 \ イベント | FailureThresholdReached | OpenTimerExpired | ProbeSucceeded | ProbeFailed |
|---|---|---|---|---|
| closed | open | - | - | - |
| open | - | half_open | - | - |
| half_open | - | - | closed | open |

閾値は設定値とします（既定: 直近10回中5回失敗でopen、open保持30秒、half_openの試行1回）。ブレーカー状態は管理画面のイベントログ画面に表示します（FR-C3）。

## 3. データモデル

物理テーブルは10個です。金額カラムはすべて`BIGINT NOT NULL`の円単位です（ADR-0006）。主キーはULID（ADR-0004）です。

| テーブル | 主要カラム | 目的・制約 |
|---|---|---|
| products | id, name, price_jpy, tokusho_snapshot | デモ商品（シード1件）。特商法表示の元データ [FR-D2] |
| payments | id (ULID), product_id, amount_jpy, refunded_jpy, state, stripe_payment_intent_id, created_at, updated_at | 決済の状態機械本体。`state`は2.1の8状態のENUM。`refunded_jpy <= amount_jpy`のCHECK制約 [FR-A2, FR-B1] |
| payment_transitions | id, payment_id, from_state, to_state, event, actor, created_at | 状態遷移の履歴（追記のみ）。詳細画面の時系列表示の源 [FR-C1, FR-D5] |
| idempotency_keys | key (UNIQUE), request_hash, response_snapshot, payment_id, expires_at | 冪等性キー。同一キー別本文は409 [FR-A4, ADR-0007] |
| ledger_entries | id, transfer_id, account, side (debit/credit), amount_jpy, payment_id, created_at | 複式簿記台帳（追記のみ）。`transfer_id`ごとに借方・貸方が対で存在し、一意制約は`UNIQUE(transfer_id, side)`の複合 [FR-D5, NFR-1, ADR-0008] |
| outbox_events | id, event_type, payload (JSONB), status, retry_count, next_attempt_at, created_at, processed_at | Outbox。`status`はpending/processing/done/failed [ADR-0009, NFR-3] |
| webhook_events | stripe_event_id (UNIQUE), event_type, payload (JSONB), status, received_at, processed_at | Webhook受信記録。UNIQUE制約が再配信の冪等化 [FR-B2] |
| admin_users / admin_sessions / audit_logs | メール、Argon2idハッシュ／セッション／操作者・操作内容・対象・日時 | 認証と監査 [FR-D3, FR-D5, ADR-0014] |

勘定科目（`ledger_entries.account`）は3つの最小セットです。`psp_receivable`（PSP未収金）、`sales`（売上）、`refunds`（返金）。手数料と入金（PSPからの実払出）の科目は、記帳の情報源となるStripeのBalance Transaction APIを扱う第3段階の対帳導入時に追加します（ADR-0008）。`transfer_id`は`{payment_id}:{イベント種別}:{連番}`の決定的導出とし、Outboxリトライの再実行では同じ値が導出されて`ON CONFLICT DO NOTHING`により二重記帳を防ぎます。台帳の不変条件「`transfer_id`単位で借方合計=貸方合計」はCIとテストの検証クエリで常時確認します（NFR-1）。

一覧検索（FR-C1、NFR-4）のために`payments(state, created_at)`、`payment_transitions(payment_id, created_at)`、`outbox_events(status, next_attempt_at)`、`webhook_events(received_at)`へインデックスを張ります。性能はシード1万件で実測します（7章）。

初期管理者は、起動時に環境変数`ADMIN_EMAIL`と`ADMIN_INITIAL_PASSWORD`が設定されており該当メールの`admin_users`行が存在しない場合にのみ作成します。作成後は環境変数を削除できます（FR-D3）。

## 4. 処理フロー

### 4.1 決済実行（FR-A1〜A4）

1. 購入者が最終確認画面（特商法6項目表示、FR-D2）で購入を確定します。
2. API層が冪等性キーを発行し`idempotency_keys`へINSERTし、同一トランザクションで`payments`をcreatedでINSERTしてコミットします。一意制約違反なら保存済み応答（`response_snapshot`）を返して終了し、未確定（`response_snapshot`がNULL）なら「処理中」の409を返します（5章）。
3. API層が同一リクエスト内でStripe PaymentIntent作成をCircuit Breaker経由・派生キー`{key}:create`付きで同期呼び出しし、得た`client_secret`を`response_snapshot`へ保存してブラウザへ返します。3DS確認（FR-A3）にはこの同期返却が必須です。タイムアウト時は同一派生キーで再送し、結果のみを取得します。
4. Circuit Breakerがopenの場合やStripe障害時は`psp-unavailable`（503、5章）を返し、購入者は後で再試行します。`payments`行はcreatedのまま残り、`checkout_expiry_minutes`経過でhousekeepingが失効させます。
5. ブラウザはStripeのトークン化フォームで3DS認証を含む確認を完了します。
6. 結果はWebhook（2.1.1の対応表）で受信し、状態遷移と台帳記帳をOutbox経由で行います。Webhook受信後の後続処理（状態遷移、台帳記帳、SSE配信）が非同期・リトライ可能な部分です。

PaymentIntent作成を同期にするのは`client_secret`の返却がチェックアウト継続の前提だからです。キャプチャと返金のAPI呼び出しも管理者応答に結果を返すため同期です（キャプチャはFR-A2の管理画面操作、返金は4.5節）。Webhook受信後の後続処理はOutbox経由の非同期で、Stripe障害からの復旧後に自動で追いつきます（NFR-2）。

### 4.2 Webhook受信（FR-B2）

1. 生ボディで署名を検証します。不正は400です。
2. `webhook_events`へのINSERTとOutboxへの処理イベント追記を同一DBトランザクションで行い、即時200を返します。重い処理はここで行いません。片方だけ成功して残ると、再配信が重複扱いで200を返しイベントが失われるため、原子性が必須です。
3. `stripe_event_id`の一意制約違反（再配信）はロールバックして200を返します。
4. リレーワーカーが状態遷移・台帳記帳・SSE配信を実行します。1件のWebhookが複数の遷移に対応する場合（2.1.1の対応表で自動キャプチャ時の`payment_intent.succeeded`がAuthorizeSucceededとCaptureに展開される場合）は、リレーワーカーが遷移イベント列を導出し、同一DBトランザクションで遷移テーブルを順に通して適用します。

### 4.3 Outboxリレー（ADR-0009、NFR-3）

`SELECT ... FOR UPDATE SKIP LOCKED`で`pending`かつ`next_attempt_at <= now()`のイベントを取得し、実行します。失敗時は`retry_count`をインクリメントし、`2^(retry_count-1)`分（1分、2分、4分、8分、16分）の指数バックオフで`next_attempt_at`を更新します。6回目の失敗で`failed`とし、管理画面に表示します（FR-C3）。初回を含む試行は最大6回で、再スケジュールは5回です（32分のバックオフには到達しません）。処理は消費側冪等（NFR-3）を前提とし、台帳記帳は決定的に導出した`transfer_id`と`UNIQUE(transfer_id, side)`制約への`ON CONFLICT DO NOTHING`で二重記帳を防ぎます（3章）。

Webhookの順序逆転（例: `charge.refunded`が`payment_intent.succeeded`より先に処理される）で遷移テーブル違反になった場合、対象`payments`が非終端状態なら一時的な順序問題とみなして通常のリトライへ回し、終端状態なら恒久的な不正として`failed`に記録します。この判定規則により、順序逆転の自己回復と真の不正遷移の検出を両立します。

### 4.4 定期ジョブ（internal/housekeeping）

1分間隔で次の3つを実行します。(1)`created`のまま`checkout_expiry_minutes`を過ぎた取引の失効とPaymentIntentキャンセル、(2)`authorized`のまま`auth_expiry_days`を過ぎた取引の失効、(3)`expires_at`を過ぎた`idempotency_keys`行の削除（ADR-0007）。(1)(2)はExpireイベントとして遷移テーブルを通します。

### 4.5 返金（FR-B1、FR-C2）

1. 管理者が返金画面で金額・理由を入力し確認モーダルで確定します（CSRFトークン必須、NFR-5）。
2. 遷移テーブルで`RefundPartial`/`RefundFull`の可否と金額上限を検証し、`audit_logs`へ操作を記録します（FR-D5）。
3. 返金連番を台帳の返金行数から決定的に導出し、派生キー`{payment_id}:refund:{返金連番}`でStripeの返金APIを同期呼び出しします（ADR-0007）。
4. Stripe成功後、同一DBトランザクションで`refunded_jpy`加算・状態遷移・台帳記帳（refunds勘定、`transfer_id`は`{payment_id}:refund:{返金連番}`）・監査ログ記録を反映します。
5. Stripe成功後にDB反映が失敗した場合、再実行では台帳が未記帳のため同じ返金連番と同じ派生キーが導出され、Stripe側の冪等性で二重返金を防ぎます。

## 5. エラー設計（ADR-0012）

JSON APIのエラーは`application/problem+json`で返します。type URIカタログは次の6つから始めます。

| type | status | 意味 | retryable |
|---|---|---|---|
| /problems/idempotency-conflict | 409 | 同一冪等性キーで異なるリクエスト本文 | false |
| /problems/idempotency-in-progress | 409 | 同一冪等性キーの先行リクエストが処理中 | true |
| /problems/invalid-transition | 409 | 遷移テーブルにない状態遷移の要求 | false |
| /problems/refund-exceeds-amount | 422 | 返金累計が取引金額を超過 | false |
| /problems/psp-unavailable | 503 | Circuit Breakerがopen | true |
| /problems/validation | 400 | 入力検証エラー | false |
| /problems/rate-limited | 429 | ログイン試行のレート制限超過 | true |
| /problems/unauthorized | 401 | 未認証 | false |

`detail`には内部情報（SQL、スタックトレース、Stripeの生エラー）を含めません。htmxへ返す画面断片のエラーは部分テンプレートで表現し、この形式の対象外です。

## 6. セキュリティ設計（FR-D1・D3・D4、NFR-5）

- 決済ページはCSP（`script-src`をStripeドメインと自ドメインに限定）とSubresource Integrityを設定します。
- 全POSTフォームにCSRFトークンを埋め込みます。セッションCookieはHttpOnly・Secure・SameSite=Laxです。
- ログイン試行はアカウント単位・IP単位でレート制限します。既定値はアカウント単位で15分間に5回失敗、IP単位で15分間に20回失敗までとし、超過は429を返します。
- ログにはカード情報が存在し得ない構成ですが、Stripeのレスポンスを生ログ出力しない方針を静的解析（ログ関数のラッパー経由強制）で担保します。
- シークレットは起動時検証し、欠落キー名のみをエラー表示します（FR-D4）。

## 7. テスト戦略との対応（NFR-6）

| 対象 | テスト | 対応する契約 |
|---|---|---|
| 状態遷移テーブル | 全セル（8状態×7イベントの許可・拒否両方）のユニットテスト | 2.1の表と1対1 |
| Circuit Breaker | 3状態×4イベントの全セルと閾値境界のユニットテスト | 2.2の表と1対1 |
| 冪等性 | 並行2リクエストの統合テスト（実PostgreSQL使用） | FR-A4の受け入れ条件 |
| 台帳不変条件 | 借方合計=貸方合計の検証クエリをテストとCIで実行 | NFR-1 |
| Webhook | 署名不正・再配信・順序逆転の統合テスト | FR-B2 |
| Outboxリレー | 二重実行・障害注入（Stripeモック停止）の統合テスト | NFR-2・NFR-3 |
| E2E | 購入→3DS→キャプチャ→返金の主要フロー（Playwright、Stripeテストカード） | FR-A1〜A3、FR-B1、FR-D2 |
| 一覧性能 | シードデータ1万件での一覧表示応答時間の実測（1秒以内） | NFR-4、3章のインデックス設計 |

統合テストはDocker ComposeのPostgreSQLを使い、モックDBは使いません（実測原則）。Stripeはstripe-mockまたはサンドボックスを使い分けます。

## 8. 運用（ADR-0015）

- 起動は`docker compose up`と`stripe listen --forward-to localhost:8080/webhooks/stripe`の2コマンドです。
- 本番化する場合の差分はTLS終端、マネージドPostgreSQLとバックアップ、シークレットマネージャ、Webhookエンドポイントの公開URL化、レート制限の外部化です。本書の範囲では設計しません。

## 9. 第3段階拡張の実装パス（要件化前の設計メモ）

- CDCリレー: Outboxリレーのポーリングを、PostgreSQLのlogical replication（`wal2json`）による`outbox_events`のINSERT捕捉へ置き換えます。Composeにコンシューマを1つ追加するだけでアプリ側の変更が不要な点を学びます（ADR-0009）。
- 対帳: Stripeの入金明細（Balance Transaction API）と`ledger_entries`を日次で突き合わせ、差分をレポートします。
- サブスクリプション・インボイス・台帳可視化・決済手段追加は、要件定義書へFRを追加してから設計します。
