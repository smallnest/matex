SHELL := /bin/bash
.DEFAULT_GOAL := help

# —— binaries ——
BIN_DIR     := bin
SERVICE     ?= demo
DRIVER      ?= postgres
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo "$(shell go env GOPATH)/bin/golangci-lint")

.PHONY: help
help: ## 列出全部目标
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## 编译全部包
	go build ./...

.PHONY: bins
bins: ## 编译服务与工具二进制到 bin/
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(SERVICE) ./cmd/$(SERVICE)
	go build -o $(BIN_DIR)/migrate ./cmd/migrate

.PHONY: run
run: ## 运行服务（完整配置）
	go run ./cmd/$(SERVICE) -conf configs/config.yaml

.PHONY: run-min
run-min: ## 零依赖运行（最小配置，不连任何外部服务）
	go run ./cmd/$(SERVICE) -conf configs/config.min.yaml

.PHONY: run-sqlite
run-sqlite: ## 带数据库零依赖运行（纯 Go SQLite；先 make migrate DRIVER=sqlite DSN=file:matex.db）
	go run ./cmd/$(SERVICE) -conf configs/config.sqlite.yaml

.PHONY: example
example: ## 运行某个示例（NAME=http|database|redis|...）
	go run ./examples/$(NAME)

.PHONY: migrate
migrate: ## 应用数据库迁移（DRIVER=postgres|mysql|sqlite DSN=...）
	go run ./cmd/migrate -driver $(DRIVER) -dsn "$(DSN)"

.PHONY: dev
dev: ## 启动本地依赖（postgres/mysql/redis/memcache/kafka）
	docker compose -f docker-compose.dev.yml up -d

.PHONY: dev-down
dev-down: ## 停止本地依赖
	docker compose -f docker-compose.dev.yml down

.PHONY: fmt
fmt: ## 格式化（排除 gen/）
	gofmt -w $$(find . -name '*.go' -not -path './gen/*')

.PHONY: vet
vet: ## 静态检查
	go vet ./...

.PHONY: lint
lint: ## golangci-lint（需 make tools）
	$(GOLANGCI_LINT) run

.PHONY: test
test: ## 全部测试（含 sqlite DB 用例）
	go test -race -count=1 ./...

.PHONY: test-short
test-short: ## 快速测试（含 sqlite DB 用例）
	go test -short -race -count=1 ./...

.PHONY: cover
cover: ## 覆盖率报告
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: ci
ci: ## 与 CI 一致的校验
	go vet ./...
	go test -short -race -count=1 ./...
	go build ./...

.PHONY: tools
tools: ## 安装 golangci-lint（protoc-gen-* 按需）
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

.PHONY: tidy
tidy: ## 整理依赖
	go mod tidy

.PHONY: docker
docker: ## 打镜像（TAG=<tag>）
	docker build -t $(SERVICE):${TAG:-latest} .

.PHONY: clean
clean: ## 清理产物
	rm -rf $(BIN_DIR) coverage.out
