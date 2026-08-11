package admin_test

import (
	"strings"
	"testing"

	"github.com/okamyuji/kessai/internal/admin"
)

func TestHashAndVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	p := admin.DefaultArgon2Params()
	// テスト時間短縮のためMemoryを小さく
	p.Memory = 8 * 1024
	p.Time = 1
	h, err := admin.HashPassword("Pa$$w0rd!", p)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("PHC接頭辞なし: %s", h)
	}
	ok, err := admin.VerifyPassword("Pa$$w0rd!", h)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("正しいパスワードでfalse")
	}
	ok, _ = admin.VerifyPassword("wrong", h)
	if ok {
		t.Fatalf("誤ったパスワードでtrue")
	}
}

func TestVerify_InvalidPHC(t *testing.T) {
	t.Parallel()
	if _, err := admin.VerifyPassword("x", "not-a-phc"); err == nil {
		t.Fatalf("PHC形式エラーを検出すべき")
	}
	if _, err := admin.VerifyPassword("x", "$argon2i$v=19$m=1,t=1,p=1$AA$BB"); err == nil {
		t.Fatalf("argon2idでないアルゴリズムを弾くべき")
	}
}
