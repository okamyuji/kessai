package idgen_test

import (
	"regexp"
	"testing"

	"github.com/okamyuji/kessai/internal/platform/idgen"
)

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestNewReturnsCrockfordBase32ULID(t *testing.T) {
	t.Parallel()
	g := idgen.NewDefault()
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		id := g.New()
		if !ulidPattern.MatchString(id) {
			t.Fatalf("ULID形式でない: %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("1000件生成で重複が発生: %q", id)
		}
		seen[id] = struct{}{}
	}
}
