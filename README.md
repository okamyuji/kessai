# kessai

決済システムに初めて触るGoエンジニアが、日本のEC決済に必要な機能を実装しながら学べる学習用アプリです。Stripeテスト環境を使って、実際に動く決済ページ・管理画面・状態遷移・複式簿記台帳までひととおり体験できます。

## モチベーション

決済ドメインはドキュメントを読んでも「実装すると何が難しいのか」が見えづらい領域です。オーソリとキャプチャの分離、冪等性キー、Webhookの再配信、Circuit Breakerによる外部障害の吸収、複式簿記台帳、監査ログといった要素は、教科書的な説明はあってもコードで手を動かさないと身につきません。

このリポジトリは次を目的にしています。

- Stripeテスト環境で「決済が1本通る」体験をローカルで完結させる
- 状態遷移テーブル・Outbox・Circuit Breaker・冪等性・Argon2id・RFC 9457 Problem Detailsを、決済という具体ドメインで使い分ける
- 決済初心者が読める設計文書（要件分析→要件定義→基本設計→ADR）と、それに対応するコード・テストをセットで提供する
- AWSに実デプロイして応答速度を実測するところまで通す

## アーキテクチャ

モジュラーモノリス + 単一PostgreSQL構成です。決済処理の複雑さは分散させず、1つのGoバイナリで完結させています。

```text
ブラウザ
  ├─ 購入ページ（templ描画 + Stripe.jsトークン化フォーム）
  └─ 管理画面（htmx + Tailwind、SSEでイベントログ自動更新）

Goアプリ（cmd/server）
  ├─ internal/payment           決済ユースケース・状態遷移テーブル
  │   ├─ statemachine           8状態×7イベントの遷移テーブル駆動
  │   ├─ idempotency            マスターキー→操作別派生キー
  │   ├─ stripeclient           stripe-go v86の薄いラッパ（Breaker連携）
  │   └─ outboxhandler          Stripeイベント→状態遷移+複式簿記記帳
  ├─ internal/ledger            複式簿記台帳（append-only、決定的transfer_id）
  ├─ internal/webhook           Stripe Webhook受信・署名検証・冪等記録
  ├─ internal/outbox            Transactional Outboxリレー（SKIP LOCKED）
  ├─ internal/housekeeping      失効ジョブ（created/authorized/idempotency_keys）
  ├─ internal/admin             Argon2id認証・返金操作・監査ログ
  ├─ internal/httpx             セキュリティヘッダ・CSRF・SSE
  ├─ internal/reconciliation    対帳（PSP未収金残高 vs Stripe入金）
  └─ internal/platform          config/logger/money(JPY BIGINT)/idgen(ULID)/breaker/problem

PostgreSQL 18（Docker Compose）
  ├─ products / payments / payment_transitions
  ├─ idempotency_keys / ledger_entries
  ├─ outbox_events / webhook_events
  └─ admin_users / admin_sessions / audit_logs
```

技術スタックの選定理由は`docs/design/adr/`のADR 1〜17に記録しています。要点だけ抜粋します。

| 領域 | 選定 | 主な理由 |
|---|---|---|
| PSP | Stripe（サンドボックス） | 公式Go SDK、豊富なドキュメント、3DS/Idempotency-Key/Webhookが揃う |
| DB | PostgreSQL 18 | ギャップロック起因のデッドロックが構造的にない、トランザクショナルDDL |
| DBアクセス | pgx + sqlc | 型安全なクエリ生成、SQLをそのまま学べる |
| 台帳 | append-only複式簿記 | 監査証跡と並行更新耐性を同時に確保 |
| 状態管理 | 8状態×7イベント遷移テーブル | if文の分岐でなく1箇所に集約、全セル網羅テスト |
| 冪等性 | サーバ発行キー+PSPへ派生キー | DB一意制約で原子的、PSPタイムアウト時も二重請求なし |
| 外部呼び出し保護 | Circuit Breaker（自前） | Closed/Open/HalfOpen、状態遷移テーブルの学習題材 |
| エラー応答 | RFC 9457 Problem Details | typeカタログで機械可読、`retryable`拡張 |
| フロント | htmx 2 + templ + Tailwind | Go単一言語、SPA不要、Stripe.jsフレームワーク非依存 |
| 認証 | Argon2id + DBセッション | OWASP推奨、CSRF/レート制限併用 |

## 動かす

### ローカル起動

```bash
# 1. DB起動
make up

# 2. マイグレーション適用（サーバ起動時にも自動適用されます）
make migrate

# 3. アプリ起動（開発時はKESSAI_INSECURE_COOKIE=1を推奨）
cp .env.example .env    # StripeキーなどをテストキーへPCで書換
go run ./cmd/server

# 4. ブラウザで確認
open http://127.0.0.1:8080/
```

### 全テスト実行

testcontainers-goがPostgreSQLコンテナを1つ起動してテスト間で共有します。Docker（macOSではColima推奨）が必要です。

```bash
make test        # ユニット+リポジトリ層+HTTP層 全19パッケージ
make crap        # CRAP<15チェック（内部パッケージ全関数）
make lint        # doclint/gofmt/vet/golangci-lint/staticcheck/govulncheck/build
```

### E2E

```bash
cd test/e2e && npm install && npx playwright install chromium
KESSAI_BASE_URL=http://127.0.0.1:8080 npm test
```

## Makefile コマンド

| ターゲット | 内容 |
|---|---|
| `make up` | Docker Compose起動（`postgres:18.4-alpine` + `stripe-mock`） |
| `make down` | Docker Compose停止 |
| `make migrate` | `db/migrations` を golang-migrate で適用 |
| `make rollback` | 直近1ステップのdown |
| `make sqlc` | `db/queries/*.sql` から Goコード生成 |
| `make fmt` | `gofmt -l -w .` |
| `make vet` | `go vet ./...` |
| `make test` | `go test -race -count=1 ./...` |
| `make coverage` | カバレッジプロファイル生成・total表示 |
| `make doclint` | 自作doclintで設計文書をチェック |
| `make golangci` | golangci-lint実行 |
| `make staticcheck` | staticcheck実行 |
| `make govulncheck` | Go脆弱性DBチェック |
| `make build` | `go build ./...` |
| `make lint` | doclint→gofmt→vet→golangci-lint→staticcheck→govulncheck→build 一括 |
| `make crap` | 循環的複雑度とカバレッジから CRAP<15 を検証 |
| `make allgates` | lint + test + crap 全通し |

## AWSデプロイ

`deploy/terraform/`にVPC/ALB/ECS Fargate/RDS PostgreSQL 18/ECR/Secrets Manager一式があります。ADR-0017に構成理由を記録しています。

```bash
cd deploy/terraform
cp terraform.tfvars.example terraform.tfvars    # Stripeテストキー等を記入
terraform init && terraform apply
# ECRへイメージpushしてECSサービスを更新（詳細はdeploy/terraform/README.md）
```

`enable_budget_mode = true` でNAT削除・Fargate SPOT・RDSバックアップ0にして月$25程度まで抑制できます。

### AWSでの実測（ap-northeast-1、ローカルMac→インターネット越し、2026-08-11時点）

Budget構成（Fargate 0.5vCPU/1GB SPOT + RDS `db.t4g.micro`）で実測しました。

| 経路 | TTFB | Total |
|---|---|---|
| GET `/`（templレンダリング + DB 2クエリ）×10連続 | 中央値 29ms | 中央値 29ms（min 24ms、max 64ms） |
| GET `/healthz`（DB非依存） | 中央値 29ms | 中央値 29ms |
| GET `/tokusho`（静的テンプレート） | 中央値 24ms | 中央値 24ms |
| 並行10リクエスト（GET `/`） | ─ | min 53ms、median 60ms、max 87ms、avg 65ms |

インターネット越しのDNS→TCP→TLS(HTTPS未使用時はTCPのみ)→ALB→ECS→RDSを含めた実測値です。RDSがprivateサブネット内で完結するため、DBクエリのラウンドトリップも安定しています。

## 設計文書

すべて`docs/`配下にあります。

| ファイル | 内容 |
|---|---|
| `docs/01-requirements-analysis.md` | 背景・目的・スコープ・機能要件のMECEツリー |
| `docs/02-requirements-specification.md` | FR14件・NFR7件（受け入れ条件付き）・MVP境界・用語集 |
| `docs/03-basic-design.md` | アーキテクチャ・状態遷移テーブル・データモデル・処理フロー・エラー設計 |
| `docs/design/adr/ADR-0001..0017` | 各設計決定の背景・根拠・影響・検討した代替案 |

## テスト方針

- ユニットテストはロジックに集中し、DB/HTTP/外部連携は実DB（testcontainers）と`httptest`で検証します。モック・スタブは対Stripeなど本当に必要な境界だけです。
- Stripeとの結合テストは公式`stripe-mock`コンテナを使い、docker composeで起動しっぱなしにできます。
- 状態遷移テーブルは全56セル（8状態×7イベント）を1つのテストで網羅します。
- Circuit Breakerは`fakeClock`で時刻注入し、`-race`で並行検証します。
- Outboxリレーは実DBで成功→done、失敗→pending+retry_count++、上限→failed+last_error、Loop→ctx.Done()終了、を全部検証します。

## 品質ゲート

コミット・デプロイの前提として、以下すべてが緑になっている必要があります。

- `gofmt -l .` 差分ゼロ
- `go vet ./...`
- `golangci-lint run` （errcheck/govet/staticcheck/unused/gosec/misspell/unconvert/gocyclo/revive/ineffassign）
- `staticcheck ./...`
- `govulncheck ./...`
- `go build ./...`
- `go test -race -count=1 ./...`
- CRAP < 15（`cmd/crapcheck` で全対象関数を確認）
- doclint（自作、境界スペース・強調記号・先送りマーカー・用語統一）

## ライセンス

学習目的のリポジトリです。ライセンス設定は今後追加します。

## 参考

### 外部ドキュメント

- 公式仕様
    - Stripe公式ドキュメント: <https://docs.stripe.com/>
    - 特定商取引法ガイド: <https://www.no-trouble.caa.go.jp/what/mailorder/>

### 著者のZenn記事（本プロジェクトの下敷きになったもの）

- 決済ドメイン・パターン
    - [Goとsunabarで送金を本番品質にする — Outbox/CircuitBreaker設計](https://zenn.dev/okamyuji/articles/go-sunabar-payments-modular-monolith-outbox)
    - [sunabar決済システムをRails8.1で再実装する — Outbox/CircuitBreakerをVanilla Railsで](https://zenn.dev/okamyuji/articles/rails-sunabar-payments-outbox-vanilla)
    - [AIエージェントは同じ決済を平気で三回叩いてくる - Idempotency-Keyで受け止めるRESTful API設計](https://zenn.dev/okamyuji/articles/idempotency-key-ai-agent-era)
    - [マイクロサービスで避けられない分散トランザクション問題をSagaパターンで解決する](https://zenn.dev/okamyuji/articles/microservices-saga-outbox-pattern)
- 状態遷移テーブル駆動
    - [Go + Reactで現場レベルの状態遷移を1つのテーブルに統合する — 13状態×15イベントを型で閉じ込める](https://zenn.dev/okamyuji/articles/golang-react-state-machine-transition-table)
    - [Railsのフラグ地獄を状態遷移テーブルで解消する — モーダルとオーバーレイの優先表示まで設計する](https://zenn.dev/okamyuji/articles/rails-state-machine-transition-table)
    - [Reactのフラグ地獄を状態遷移テーブルで解消する — Discriminated Union×テーブル駆動設計の実践](https://zenn.dev/okamyuji/articles/react-state-pattern-finite-state-machine)
- エラー応答・API設計
    - [RFC 9457 Problem DetailsをGoでClean Architectureで実装する](https://zenn.dev/okamyuji/articles/rfc9457-go-clean-architecture)
    - [Go標準ライブラリで作るREST APIサーバー：JWT認証とミドルウェアパターンの実践](https://zenn.dev/okamyuji/articles/golang-rest-api-standard-library)
- 外部呼び出しの防御
    - [キャッシュ期限切れでDBが落ちる前に - Go+Redisで学ぶ防御戦略と選び方](https://zenn.dev/okamyuji/articles/cache-stampede-four-strategies-go-redis)
- PostgreSQL・データ
    - [MySQL + GoでN+1問題を32倍高速化した話とInnoDB Buffer Poolの活用](https://zenn.dev/okamyuji/articles/mysql-n-plus-1)
    - [OpenAIほどの規模ではなくてもRDB設計はPrimaryを守る考え方でだいたい対応できる](https://zenn.dev/okamyuji/articles/mysql-read-replica-write-ahead-htmx-go)
- Go実装の下敷き
    - [Go 1.25で始める"本番に強い"開発](https://zenn.dev/okamyuji/articles/golang-125-guideline)
- フロントエンド (htmx + Go + templ)
    - [Slackの骨格は何か - チャットと連携ハブをGoとhtmxで再実装する](https://zenn.dev/okamyuji/articles/slack-skeleton-go-htmx)
- テスト・品質
    - [AIにテストを書かせてカバレッジ100%、それでもバグが出るのはなぜか](https://zenn.dev/okamyuji/articles/ai-tests-coverage-blindspot)
    - [テスト容易性を設計に組み込む - Seamで実現する現実的なユニットテスト戦略](https://zenn.dev/okamyuji/articles/seam-testing-guide)
- セキュリティ・CI/CD
    - [GitHubに機密情報をpushしてしまった日のために — 無効化、履歴除去、多層防御の組み立て方](https://zenn.dev/okamyuji/articles/github-secret-removal-multi-layer-defense)
    - [たった1つのGitHub Actionsトークンから全コードを盗まれない設計 - Grafanaでの流出から学んだ設計原則](https://zenn.dev/okamyuji/articles/grafana-github-actions-token-supply-chain)
    - [CIを再利用可能なワークフローにして複数のGitリポジトリを一元管理する](https://zenn.dev/okamyuji/articles/github-reusable-workflows-ci-unification)
    - [コンテナのセキュリティを高めるDHI - node/python/ruby/goでの比較](https://zenn.dev/okamyuji/articles/docker-hardened-images-dockerfile-migration)
- デプロイ・運用
    - [セッション管理を考慮したBlue-Greenデプロイメントの実装](https://zenn.dev/okamyuji/articles/blue-green-deployment-with-session-management)
    - [AWS API Gatewayを入れる条件とコストの釣り合いを考える](https://zenn.dev/okamyuji/articles/fintech-mobile-bff-api-gateway-cost-design)
