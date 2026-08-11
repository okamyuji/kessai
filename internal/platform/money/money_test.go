package money_test

import (
	"errors"
	"testing"

	"github.com/okamyuji/kessai/internal/platform/money"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		amount  int64
		wantErr error
	}{
		{"zero ok", 0, nil},
		{"positive ok", 1234, nil},
		{"negative rejected", -1, money.ErrNegative},
		{"max int64 ok", 1<<62 - 1, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := money.New(tc.amount)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v want %v", err, tc.wantErr)
			}
			if err == nil && m.Int64() != tc.amount {
				t.Fatalf("Int64=%d want %d", m.Int64(), tc.amount)
			}
		})
	}
}

func TestString(t *testing.T) {
	t.Parallel()
	tests := map[int64]string{
		0:          "0円",
		1:          "1円",
		999:        "999円",
		1000:       "1,000円",
		12345:      "12,345円",
		123456:     "123,456円",
		1234567:    "1,234,567円",
		1000000000: "1,000,000,000円",
	}
	for amount, want := range tests {
		t.Run(want, func(t *testing.T) {
			t.Parallel()
			got := money.MustNew(amount).String()
			if got != want {
				t.Fatalf("String()=%q want %q", got, want)
			}
		})
	}
}

func TestAdd(t *testing.T) {
	t.Parallel()
	a := money.MustNew(100)
	b := money.MustNew(250)
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("add err=%v", err)
	}
	if sum.Int64() != 350 {
		t.Fatalf("sum=%d want 350", sum.Int64())
	}

	max := money.MustNew(1<<62 + (1<<62 - 1)) // 実質 MaxInt64 相当
	if _, err := max.Add(money.MustNew(1)); err == nil {
		t.Fatalf("overflow を検出すべき")
	}
}

func TestSub(t *testing.T) {
	t.Parallel()
	a := money.MustNew(500)
	b := money.MustNew(200)
	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("sub err=%v", err)
	}
	if diff.Int64() != 300 {
		t.Fatalf("diff=%d want 300", diff.Int64())
	}
	if _, err := b.Sub(a); err == nil {
		t.Fatalf("負値になるSubはエラーを返すべき")
	}
}

func TestEqualAndLessThan(t *testing.T) {
	t.Parallel()
	a := money.MustNew(100)
	b := money.MustNew(100)
	c := money.MustNew(101)
	if !a.Equal(b) {
		t.Fatalf("Equal must be true")
	}
	if a.Equal(c) {
		t.Fatalf("Equal must be false for different amounts")
	}
	if !a.LessThan(c) {
		t.Fatalf("LessThan must be true")
	}
	if c.LessThan(a) {
		t.Fatalf("LessThan must be false")
	}
}

func TestMustNewPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("MustNew(-1) はpanicするべき")
		}
	}()
	_ = money.MustNew(-1)
}
