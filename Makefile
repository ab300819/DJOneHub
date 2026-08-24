.DEFAULT_GOAL := help

GO      ?= go
VERSION ?= dev
ARCH    ?= $(shell uname -m)
PKGS    := ./cmd/... ./core/... ./internal/... ./pkg/...

.PHONY: help
help: ## 列出可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 按宿主机架构构建到 dist/（开发用）
	./scripts/build-macos.sh

.PHONY: release
release: ## 构建指定平台发行包，用法：make release VERSION=v0.1.0 [ARCH=arm64]
	./scripts/package-macos.sh $(VERSION) $(ARCH)

.PHONY: test
test: ## 运行全部测试
	$(GO) test $(PKGS)

.PHONY: vet
vet: ## 运行 go vet
	$(GO) vet $(PKGS)

.PHONY: fmt
fmt: ## 格式化源码
	$(GO) fmt $(PKGS)

.PHONY: fmt-check
fmt-check: ## 检查格式，有未格式化文件则失败
	@unformatted=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未格式化，请运行 make fmt："; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: lint
lint: ## 运行 golangci-lint（需自行安装）
	@command -v golangci-lint >/dev/null 2>&1 \
		|| { echo "未找到 golangci-lint，安装：go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }
	golangci-lint run

.PHONY: tidy
tidy: ## 整理 go.mod / go.sum
	$(GO) mod tidy

.PHONY: check
check: fmt-check vet lint test ## 提交前的完整检查

.PHONY: clean
clean: ## 清理构建产物
	rm -rf dist
