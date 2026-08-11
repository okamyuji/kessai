package httpx_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/okamyuji/kessai/internal/httpx"
)

func TestSSEHub_PublishReachesSubscriber(t *testing.T) {
	t.Parallel()
	hub := httpx.NewSSEHub(slog.New(slog.NewTextHandler(io.Discard, nil)), 4)
	srv := httptest.NewServer(http.HandlerFunc(hub.Handler))
	defer srv.Close()

	// httptest経由でGET → keep-alive読取
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Publishが購読者に届くまで少し待つ
	go func() {
		time.Sleep(100 * time.Millisecond)
		hub.Publish("payment.updated", `{"id":"P1"}`)
	}()
	buf := make([]byte, 2048)
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		n, _ := resp.Body.Read(buf)
		got += string(buf[:n])
		if strings.Contains(got, "payment.updated") {
			break
		}
	}
	if !strings.Contains(got, "payment.updated") {
		t.Fatalf("イベントが届いていない: %q", got)
	}
}
