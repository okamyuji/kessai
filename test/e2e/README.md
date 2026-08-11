# E2Eテスト

## 前提

- docker compose で PostgreSQL が5433に起動
- `db/migrations` を適用済み
- `products` に1件シード済み（テスト冒頭に手動で `psql` 等でINSERTしてください）
- `cmd/server` が 127.0.0.1:8080 で稼働中

## セットアップ

```bash
cd test/e2e
npm install
npx playwright install chromium
```

## 実行

```bash
KESSAI_BASE_URL=http://127.0.0.1:8080 npx playwright test
```

## Stripeテストカードで実際に決済を通す

Playwright テストではモックしません。Stripe Testモードのカード番号（`4242 4242 4242 4242`、`4000 0025 0000 3155` 3DS必須、`4000 0000 0000 9995` 残高不足）を Payment Element に入力して動作を確認します。3DS認証チャレンジは Stripe が提供する模擬画面で `Complete` を押します。

## 競合パターン

- 同一冪等性キーでの二重POST（Go側統合テストで verify、E2E補足）
- Webhook 順序逆転（Stripe CLI `trigger` を並行実行して確認）
- Circuit Breaker open時の 503 応答（stripeclient をモード切り替え可能な設定にした後に検証）
