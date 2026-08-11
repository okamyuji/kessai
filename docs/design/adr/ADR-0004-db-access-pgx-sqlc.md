# ADR-0004 DBアクセスはpgxとsqlcとする

## ステータス

Accepted（2026-08-11）

## コンテキスト

GoからPostgreSQLへアクセスする手段として、ドライバ（pgx、lib/pq）とクエリ層（sqlc、GORM、sqlx、手書き）の選定が必要です。

## 決定

ドライバはpgx v5系（2026-08-11時点の最新タグv5.10.0をGitHub APIで実測）、クエリ層はsqlc v1.31系（同v1.31.1）を採用します。sqlcはpgx/v5モードで使います。Goツールチェーンは1.26系（2026-08-11時点の最新安定版1.26.5をgo.devのdl APIとローカル`go version`で実測）とします。主キーのID生成は時系列ソート可能なULID（`oklog/ulid`）とします。

## 根拠

lib/pqはメンテナンスモードでありpgxがデファクトです。sqlcはSQLファイルから型安全なGoコードを生成するため、初心者がSQLそのものを学びながら型の恩恵を受けられます。ORMの隠蔽（GORM）は決済学習の目的（SQLとロックを理解する）に反します。

## 影響

クエリはdb/queries/配下のSQLファイルに集約され、sqlc generateで再生成します。SELECT FOR UPDATEやSKIP LOCKEDを明示的にSQLで書きます。

## 検討した代替案

GORMは自動生成SQLがロック挙動を隠すため却下しました。sqlxは型安全性がsqlcに劣ります。database/sql直書きはボイラープレートが多く、pgx固有機能へも届きません。
