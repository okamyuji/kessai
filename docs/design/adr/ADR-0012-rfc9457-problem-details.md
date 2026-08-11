# ADR-0012 APIエラー応答はRFC 9457 Problem Detailsとする

## ステータス

Accepted（2026-08-11）

## コンテキスト

決済APIのエラーは「誰の問題で、リトライしてよいか」を機械可読に伝える必要があります。Zenn記事「RFC 9457とGoクリーンアーキテクチャ」の知見を織り込みます。

## 決定

JSONを返すAPIエンドポイントのエラー応答はRFC 9457のProblem Details（application/problem+json）とします。type・title・status・detail・instanceに加え、拡張メンバーとして`retryable`（真偽値）と`payment_id`を定義します。htmxへ返すHTML断片のエラーはこの対象外とし、部分テンプレートで表現します。

## 根拠

標準形式によりエラーの語彙が統一され、内部実装の詳細（スタックトレース、SQL）を漏らさない出口を1箇所に集約できます。

## 影響

エラー種別ごとのtype URIカタログ（例 /problems/idempotency-conflict、/problems/invalid-transition）を03-basic-designで定義します。セキュリティ上、detailに内部情報を含めないレビュー観点を追加します。

## 検討した代替案

独自JSON形式は標準の利点を捨てるだけの理由がありません。HTTPステータスコードのみでは409の原因（冪等性衝突か遷移違反か）を区別できません。
