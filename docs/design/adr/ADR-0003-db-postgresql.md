# ADR-0003 データベースはPostgreSQL 18系とする

## ステータス

Accepted（2026-08-11）

## コンテキスト

決済台帳は同一アカウントへの並行更新が集中するため、ロック挙動と整合性保証がDB選定の中心軸です。候補はMySQL（InnoDB）とPostgreSQLです。

## 決定

PostgreSQL 18系を採用します（2026-08-11時点の最新安定系列18、最新パッチ18.4をendoflife.date APIで実測）。

## 根拠

1. MySQLのREPEATABLE READで働くギャップロックは、範囲条件付きSELECT FOR UPDATEで意図しない範囲をロックしデッドロックを誘発しやすい性質があります。PostgreSQLのMVCCにはこのパターンが構造的に存在しません。
2. トランザクショナルDDLによりマイグレーション失敗が自動ロールバックされます。
3. SELECT FOR UPDATE SKIP LOCKEDでOutboxリレーとジョブキューをDBだけで実装でき、外部ミドルウェアを増やしません。
4. 決済企業の一次情報としてAdyenのPostgreSQL本番運用を確認しています。
5. Goエコシステム（pgx + sqlc）の親和性が高いです（ADR-0004）。

## 影響

VACUUM等のPostgreSQL固有の運用知識が必要になります。Docker Composeのイメージはpostgres:18系を固定します。

## 検討した代替案

MySQLは運用人材の層が厚い利点がありますが、上記1・2の決済適性で劣るため見送りました。決済クリティカルパスで既定分離レベルに依存しない明示的ロック戦略を取る方針は、どちらを選んでも共通の必須事項として03-basic-designに記載します。
