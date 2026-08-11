// Package admin 管理画面の認証・認可、返金操作、監査ログを提供します（ADR-0014、FR-D3・FR-D5）。
// パスワードは Argon2id（OWASP推奨）でハッシュ化し、セッションはDB保存＋HttpOnly Cookieで管理します。
package admin

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/okamyuji/kessai/internal/platform/idgen"
	"github.com/okamyuji/kessai/internal/platform/sqlc"
)

// Argon2Params OWASP推奨の既定値。CPU/メモリ負荷は運用環境に合わせて調整可能。
type Argon2Params struct {
	Time    uint32 // 反復回数
	Memory  uint32 // KiB
	Threads uint8
	KeyLen  uint32
	SaltLen uint32
}

// DefaultArgon2Params OWASP Argon2id 推奨（2024年時点）: Time=2, Memory=64MiB, Threads=1, KeyLen=32
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{Time: 2, Memory: 64 * 1024, Threads: 1, KeyLen: 32, SaltLen: 16}
}

// HashPassword Argon2idハッシュを生成し、PHC文字列（$argon2id$...）形式で返します
func HashPassword(password string, p Argon2Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := cryptorand.Read(salt); err != nil {
		return "", fmt.Errorf("admin: salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash))
	return encoded, nil
}

// VerifyPassword PHC文字列とパスワードを比較します（定数時間）
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, hash, err := decodeArgonPHC(encoded)
	if err != nil {
		return false, err
	}
	other := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, safeUint32(len(hash)))
	return subtle.ConstantTimeCompare(hash, other) == 1, nil
}

// decodeArgonPHC $argon2id$v=19$m=65536,t=2,p=1$SALT$HASH をパース
func decodeArgonPHC(encoded string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Argon2Params{}, nil, nil, errors.New("admin: 不明なPHC形式")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return Argon2Params{}, nil, nil, fmt.Errorf("admin: version: %w", err)
	}
	if version != argon2.Version {
		return Argon2Params{}, nil, nil, fmt.Errorf("admin: argon version不一致 %d", version)
	}
	var mem uint32
	var t uint32
	var thr uint8
	// m,t,p を1つずつパース
	kv := strings.Split(parts[3], ",")
	if len(kv) != 3 {
		return Argon2Params{}, nil, nil, errors.New("admin: params形式")
	}
	m64, err := strconv.ParseUint(strings.TrimPrefix(kv[0], "m="), 10, 32)
	if err != nil {
		return Argon2Params{}, nil, nil, err
	}
	mem = uint32(m64)
	t64, err := strconv.ParseUint(strings.TrimPrefix(kv[1], "t="), 10, 32)
	if err != nil {
		return Argon2Params{}, nil, nil, err
	}
	t = uint32(t64)
	p64, err := strconv.ParseUint(strings.TrimPrefix(kv[2], "p="), 10, 8)
	if err != nil {
		return Argon2Params{}, nil, nil, err
	}
	thr = uint8(p64)
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Argon2Params{}, nil, nil, err
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Argon2Params{}, nil, nil, err
	}
	return Argon2Params{Time: t, Memory: mem, Threads: thr, KeyLen: safeUint32(len(hash)), SaltLen: safeUint32(len(salt))}, salt, hash, nil
}

// SessionStore adminセッション操作の抽象
type SessionStore interface {
	CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error)
	LookupSession(ctx context.Context, sessionID string) (string, error) // returns userID
	DestroySession(ctx context.Context, sessionID string) error
}

// PGSessionStore sqlc.QueriesベースのSessionStore実装
type PGSessionStore struct {
	Q   *sqlc.Queries
	IDs idgen.Generator
	Now func() time.Time
}

// NewPGSessionStore コンストラクタ
func NewPGSessionStore(q *sqlc.Queries, ids idgen.Generator) *PGSessionStore {
	return &PGSessionStore{Q: q, IDs: ids, Now: time.Now}
}

// CreateSession 新規セッションID発行
func (s *PGSessionStore) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	sid := s.IDs.New()
	err := s.Q.CreateAdminSession(ctx, sqlc.CreateAdminSessionParams{
		ID: sid, AdminUserID: userID,
		ExpiresAt: pgtype.Timestamptz{Time: s.Now().Add(ttl), Valid: true},
	})
	if err != nil {
		return "", err
	}
	return sid, nil
}

// LookupSession 期限内セッションのユーザIDを返す
func (s *PGSessionStore) LookupSession(ctx context.Context, sid string) (string, error) {
	row, err := s.Q.GetAdminSession(ctx, sid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrSessionNotFound
		}
		return "", err
	}
	return row.AdminUserID, nil
}

// DestroySession ログアウト
func (s *PGSessionStore) DestroySession(ctx context.Context, sid string) error {
	return s.Q.DeleteAdminSession(ctx, sid)
}

// ErrSessionNotFound セッションが存在しない/期限切れ
var ErrSessionNotFound = errors.New("admin: session not found or expired")

// safeUint32 int→uint32変換をクランプします（Argon2ハッシュ・ソルト長でのオーバーフロー警告回避）
func safeUint32(n int) uint32 {
	if n < 0 {
		return 0
	}
	if uint64(n) > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(n)
}
