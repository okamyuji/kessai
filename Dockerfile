# kessai サーバのマルチステージビルド。
# ビルド段: Go 1.26のalpineイメージで静的リンク、実行段: 最小限のdistroless。
FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO無効・静的リンクで distroless に載せられるようにする
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/kessai-server ./cmd/server

# 実行段: distroless static（root化されないshellなしイメージ）
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/kessai-server /app/kessai-server
# 静的アセットとマイグレーションは同梱してECS側で追加DL不要にする
COPY --from=builder /src/web/static /app/web/static
COPY --from=builder /src/web/templates /app/web/templates
COPY --from=builder /src/db/migrations /app/db/migrations
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/kessai-server"]
