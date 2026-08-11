// Package pgcontainer testcontainers-goでPostgreSQLコンテナを起動し、
// テスト間で使い回すためのヘルパです。
//
// 使い方:
//
//	var shared *pgcontainer.Container
//
//	func TestMain(m *testing.M) {
//	    ctx := context.Background()
//	    c, err := pgcontainer.Start(ctx)
//	    if err != nil { log.Fatal(err) }
//	    shared = c
//	    code := m.Run()
//	    _ = shared.Stop(ctx)
//	    os.Exit(code)
//	}
//
//	func TestXxx(t *testing.T) {
//	    ctx := context.Background()
//	    if err := shared.Reset(ctx); err != nil { t.Fatalf("reset: %v", err) }
//	    pool := shared.Pool()
//	    // ... test body ...
//	}
//
// 特徴:
//   - コンテナはパッケージ単位で1つ、TestMainから起動して使い回します。
//   - Reset() は全テーブルをTRUNCATEしてテスト間の独立性を回復します。
//   - migrationファイルはコピーせず、file:// URLで golang-migrate に直渡しします。
//   - コンテナ終了時にDBは破棄されるため、down マイグレーションは不要です。
//   - リポジトリのdb/migrationsは、実行時のGoファイルからの相対で解決します。
package pgcontainer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/golang-migrate/migrate/v4"

	// migrate はドライバをblankインポートで登録します
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Container 起動済みPostgreSQLコンテナと接続プールを保持します
type Container struct {
	pg   *tcpg.PostgresContainer
	pool *pgxpool.Pool
	url  string
	once sync.Once
}

// Start PostgreSQL 18のコンテナを起動し、マイグレーションを適用します。
// 呼び出し側は Stop でコンテナを破棄します（testcontainersのReaperが最終的な後片付けをします）。
func Start(ctx context.Context) (*Container, error) {
	migDir, err := resolveMigrationsDir()
	if err != nil {
		return nil, err
	}
	pg, err := tcpg.Run(ctx,
		"postgres:18.4-alpine",
		tcpg.WithDatabase("kessai_test"),
		tcpg.WithUsername("kessai"),
		tcpg.WithPassword("kessai_test"), // #nosec G101 -- テスト専用の固定値
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("pgcontainer: 起動失敗: %w", err)
	}
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("pgcontainer: DSN取得失敗: %w", err)
	}
	if err := applyMigrations(dsn, migDir); err != nil {
		_ = pg.Terminate(ctx)
		return nil, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = pg.Terminate(ctx)
		return nil, fmt.Errorf("pgcontainer: プール作成失敗: %w", err)
	}
	return &Container{pg: pg, pool: pool, url: dsn}, nil
}

// Pool 共有pgxpool.Pool。テスト内で自由に使えます。
func (c *Container) Pool() *pgxpool.Pool { return c.pool }

// DSN 接続文字列（診断用途）
func (c *Container) DSN() string { return c.url }

// Reset 全アプリケーションテーブルをTRUNCATEします。
// スキーマや型定義は残るので、コンテナを使い回してもテストは独立に動きます。
func (c *Container) Reset(ctx context.Context) error {
	if c == nil || c.pool == nil {
		return errors.New("pgcontainer: 未起動")
	}
	// public.tables を動的に収集して1本のTRUNCATEにまとめます。
	rows, err := c.pool.Query(ctx, `
        SELECT quote_ident(tablename)
        FROM pg_tables
        WHERE schemaname = 'public' AND tablename <> 'schema_migrations'
    `)
	if err != nil {
		return fmt.Errorf("pgcontainer: table列挙失敗: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	stmt := "TRUNCATE " + join(names, ", ") + " RESTART IDENTITY CASCADE"
	if _, err := c.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgcontainer: TRUNCATE失敗: %w", err)
	}
	return nil
}

// Stop プールをクローズし、コンテナを破棄します。TestMain の最後で呼びます。
func (c *Container) Stop(ctx context.Context) error {
	var err error
	c.once.Do(func() {
		if c.pool != nil {
			c.pool.Close()
		}
		if c.pg != nil {
			err = c.pg.Terminate(ctx)
		}
	})
	return err
}

// applyMigrations golang-migrateライブラリAPIで、db/migrationsをfile://で直接適用します。
// コンテナへのファイルコピーは行いません（migrate はドライバ経由でDBへ直接SQLを送ります）。
func applyMigrations(dsn, migDir string) error {
	m, err := migrate.New("file://"+migDir, dsn)
	if err != nil {
		return fmt.Errorf("pgcontainer: migrate.New: %w", err)
	}
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			// クローズ失敗は致命ではないので握りつぶすが、診断のために標準エラーへ流したいときは
			// 呼び出し側でlog.Printfを差し込むことを検討する
			_ = srcErr
			_ = dbErr
		}
	}()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("pgcontainer: migrate.Up: %w", err)
	}
	return nil
}

// resolveMigrationsDir このソースファイルの位置からリポジトリルート/db/migrations を絶対パスで返します。
// テスト実行時のCWDに依存しないため、どのパッケージから使ってもmigrationを見つけられます。
func resolveMigrationsDir() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("pgcontainer: runtime.Caller失敗")
	}
	// このファイル: <repo>/internal/testsupport/pgcontainer/pgcontainer.go
	// migrations : <repo>/db/migrations
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	abs, err := filepath.Abs(filepath.Join(repoRoot, "db", "migrations"))
	if err != nil {
		return "", fmt.Errorf("pgcontainer: 絶対パス化失敗: %w", err)
	}
	return abs, nil
}

// join strings.Joinを避けて外部依存を最小にしたい場合の内製版
func join(ss []string, sep string) string {
	switch len(ss) {
	case 0:
		return ""
	case 1:
		return ss[0]
	}
	n := len(sep) * (len(ss) - 1)
	for _, s := range ss {
		n += len(s)
	}
	b := make([]byte, 0, n)
	b = append(b, ss[0]...)
	for _, s := range ss[1:] {
		b = append(b, sep...)
		b = append(b, s...)
	}
	return string(b)
}
