.PHONY: build build-embed generate test test-unit test-integration test-e2e

VERSION ?= $(shell ./scripts/resolve-version.sh)
LDFLAGS ?= -s -w -X main.Version=$(VERSION)

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -trimpath -o bin/server ./cmd/server

# 发布形态构建：内嵌管理面板前端（internal/web/dist，由前端构建产出）。
# 不带 -tags=embed 的产物没有前端资源，/ 与 /login 全 404（面板消失、API 正常），
# 2026-09-18 生产事故即由此而来——部署一律用本目标。
build-embed:
	@test -f internal/web/dist/index.html || (echo "缺少 internal/web/dist，先构建前端：pnpm --dir ../frontend run build" >&2; exit 1)
	CGO_ENABLED=0 go build -tags=embed -ldflags="$(LDFLAGS)" -trimpath -o bin/server ./cmd/server

generate:
	go generate ./ent
	go generate ./cmd/server

test:
	go test ./...
	golangci-lint run ./...

test-unit:
	go test -tags=unit ./...

test-integration:
	go test -tags=integration ./...

test-e2e:
	./scripts/e2e-test.sh

test-e2e-local:
	go test -tags=e2e -v -timeout=300s ./internal/integration/...
