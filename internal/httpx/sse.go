package httpx

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
)

// SSEHub 単一プロセス内での Server-Sent Events 配信ハブです（03-basic-design FR-C3）。
// PublishでイベントをJSONとして全購読者へ送信、Handlerでクライアントを購読させます。
type SSEHub struct {
	mu      sync.RWMutex
	subs    map[uint64]chan string
	nextID  atomic.Uint64
	logger  *slog.Logger
	bufSize int
}

// NewSSEHub Hubを構築します。bufSizeはクライアントごとの送信バッファ数です。
func NewSSEHub(logger *slog.Logger, bufSize int) *SSEHub {
	if bufSize <= 0 {
		bufSize = 16
	}
	return &SSEHub{subs: map[uint64]chan string{}, logger: logger, bufSize: bufSize}
}

// Publish イベント種別と本文（JSON文字列を推奨）を全購読者へ配信します。
// バッファ満杯の購読者にはスキップ（配信遅延を避ける）します。
func (h *SSEHub) Publish(eventType, data string) {
	msg := "event: " + eventType + "\ndata: " + data + "\n\n"
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, ch := range h.subs {
		select {
		case ch <- msg:
		default:
			h.logger.Debug("sse: subscriber slow, drop")
		}
	}
}

// Handler SSEクライアントを購読させます。Context終了で切断します。
func (h *SSEHub) Handler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan string, h.bufSize)
	id := h.nextID.Add(1)
	h.mu.Lock()
	h.subs[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, id)
		h.mu.Unlock()
		close(ch)
	}()

	// 初回接続確認
	if _, err := fmt.Fprint(w, "event: ready\ndata: {}\n\n"); err != nil {
		return
	}
	fl.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if _, err := fmt.Fprint(w, msg); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
