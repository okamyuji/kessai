SHELL := /bin/bash
DBURL ?= postgres://kessai:kessai_dev@localhost:5433/kessai?sslmode=disable

# testcontainers-go は DOCKER_HOST を見て docker daemon を検出します。
# Colima 利用時のみ検出できない場合があるため、colimaソケットが存在するときはそれを既定にします。
COLIMA_SOCK := $(HOME)/.colima/default/docker.sock
ifeq ($(wildcard $(COLIMA_SOCK)),$(COLIMA_SOCK))
export DOCKER_HOST ?= unix://$(COLIMA_SOCK)
export TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE ?= /var/run/docker.sock
endif

.PHONY: up down migrate rollback sqlc test lint doclint tidy fmt vet coverage

up:
	docker compose up -d

down:
	docker compose down

migrate:
	migrate -path db/migrations -database "$(DBURL)" up

rollback:
	migrate -path db/migrations -database "$(DBURL)" down 1

sqlc:
	sqlc -f sqlc.yaml generate

fmt:
	gofmt -l -w .

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

coverage:
	go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

doclint:
	go run ./cmd/doclint docs

gofmt-check:
	@diff -u <(echo -n) <(gofmt -l .) || (echo 'gofmt差分あり'; exit 1)

golangci:
	golangci-lint run ./...

staticcheck:
	staticcheck ./...

build:
	go build ./...

govulncheck:
	govulncheck ./...

lint: doclint gofmt-check vet golangci staticcheck govulncheck build
	@echo lint ok

crap:
	go test -coverprofile=coverage.out -covermode=atomic ./... > /dev/null
	go run ./cmd/crapcheck -profile=coverage.out -path=internal -exclude-prefix=internal/platform/sqlc,internal/payment/stripeclient,internal/testsupport

allgates: lint test crap
	@echo all gates ok

tidy:
	go mod tidy
